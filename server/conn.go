// Copyright 2013 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

// The MIT License (MIT)
//
// Copyright (c) 2014 wandoulabs
// Copyright (c) 2014 siddontang
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of
// this software and associated documentation files (the "Software"), to deal in
// the Software without restriction, including without limitation the rights to
// use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
// the Software, and to permit persons to whom the Software is furnished to do so,
// subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"time"

	log "github.com/golang/glog"
	"github.com/juju/errors"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/util/arena"

	"github.com/youtube/vitess/go/hack"
	"github.com/youtube/vitess/go/sqltypes"
	"github.com/youtube/vitess/go/vt/proto/query"
)

var defaultCapability = mysql.ClientLongPassword | mysql.ClientLongFlag |
	mysql.ClientConnectWithDB | mysql.ClientProtocol41 |
	mysql.ClientTransactions | mysql.ClientSecureConnection | mysql.ClientFoundRows |
	mysql.ClientMultiStatements | mysql.ClientMultiResults | mysql.ClientLocalFiles |
	mysql.ClientConnectAtts

// clientConn represents a connection between server and client, it maintains connection specific state,
// handles client query.
type clientConn struct {
	pkt          *packetIO // a helper to read and write data in packet format.
	conn         net.Conn
	server       *Server         // a reference of server instance.
	capability   uint32          // client capability affects the way server handles client request.
	connectionID uint32          // atomically allocated by a global variable, unique in process scope.
	collation    uint8           // collation used by client, may be different from the collation used by database.
	user         string          // user of the client.
	dbname       string          // default database name.
	salt         []byte          // random bytes used for authentication.
	alloc        arena.Allocator // an memory allocator for reducing memory allocation.
	lastCmd      string          // latest sql query string, currently used for logging error.
	//	ctx          IContext             // an interface to execute sql statements.
	attrs      map[string]string // attributes parsed from client handshake response, not used for now.
	vtgate     *VtConn
	lastResult *sqltypes.Result
}

func (cc *clientConn) String() string {
	collationStr := mysql.Collations[cc.collation]
	return fmt.Sprintf("id:%d, addr:%s status:%d, collation:%s, user:%s",
		cc.connectionID, cc.conn.RemoteAddr(), cc.Status(), collationStr, cc.user,
	)
}

// handshake works like TCP handshake, but in a higher level, it first writes initial packet to client,
// during handshake, client and server negotiate compatible features and do authentication.
// After handshake, client can send sql query to server.
func (cc *clientConn) handshake() error {
	if err := cc.writeInitialHandshake(); err != nil {
		return errors.Trace(err)
	}
	if err := cc.readHandshakeResponse(); err != nil {
		cc.writeError(err)
		return errors.Trace(err)
	}
	data := cc.alloc.AllocWithLen(4, 32)
	data = append(data, mysql.OKHeader)
	data = append(data, 0, 0)
	if cc.capability&mysql.ClientProtocol41 > 0 {
		data = append(data, dumpUint16(mysql.ServerStatusAutocommit)...)
		data = append(data, 0, 0)
	}

	err := cc.writePacket(data)
	cc.pkt.sequence = 0
	if err != nil {
		return errors.Trace(err)
	}

	return errors.Trace(cc.flush())
}

func (cc *clientConn) Close() error {
	cc.server.rwlock.Lock()
	delete(cc.server.clients, cc.connectionID)
	cc.server.rwlock.Unlock()
	cc.conn.Close()
	return nil
}

// writeInitialHandshake sends server version, connection ID, server capability, collation, server status
// and auth salt to the client.
func (cc *clientConn) writeInitialHandshake() error {
	data := make([]byte, 4, 128)

	// min version 10
	data = append(data, 10)
	// server version[00]
	data = append(data, mysql.ServerVersion...)
	data = append(data, 0)
	// connection id
	data = append(data, byte(cc.connectionID), byte(cc.connectionID>>8), byte(cc.connectionID>>16), byte(cc.connectionID>>24))
	// auth-plugin-data-part-1
	data = append(data, cc.salt[0:8]...)
	// filler [00]
	data = append(data, 0)
	// capability flag lower 2 bytes, using default capability here
	data = append(data, byte(defaultCapability), byte(defaultCapability>>8))
	// charset, utf-8 default
	data = append(data, uint8(mysql.DefaultCollationID))
	//status
	data = append(data, dumpUint16(mysql.ServerStatusAutocommit)...)
	// below 13 byte may not be used
	// capability flag upper 2 bytes, using default capability here
	data = append(data, byte(defaultCapability>>16), byte(defaultCapability>>24))
	// filler [0x15], for wireshark dump, value is 0x15
	data = append(data, 0x15)
	// reserved 10 [00]
	data = append(data, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	// auth-plugin-data-part-2
	data = append(data, cc.salt[8:]...)
	// filler [00]
	data = append(data, 0)
	err := cc.writePacket(data)
	if err != nil {
		return errors.Trace(err)
	}
	return errors.Trace(cc.flush())
}

func (cc *clientConn) readPacket() ([]byte, error) {
	return cc.pkt.readPacket()
}

func (cc *clientConn) writePacket(data []byte) error {
	return cc.pkt.writePacket(data)
}

type handshakeResponse41 struct {
	Capability uint32
	Collation  uint8
	User       string
	DBName     string
	Auth       []byte
	Attrs      map[string]string
}

func handshakeResponseFromData(packet *handshakeResponse41, data []byte) error {
	pos := 0
	// capability
	capability := binary.LittleEndian.Uint32(data[:4])
	packet.Capability = capability
	pos += 4
	// skip max packet size
	pos += 4
	// charset, skip, if you want to use another charset, use set names
	packet.Collation = data[pos]
	pos++
	// skip reserved 23[00]
	pos += 23
	// user name
	packet.User = string(data[pos : pos+bytes.IndexByte(data[pos:], 0)])
	pos += len(packet.User) + 1

	if capability&mysql.ClientPluginAuthLenencClientData > 0 {
		// MySQL client sets the wrong capability, it will set this bit even server doesn't
		// support ClientPluginAuthLenencClientData.
		// https://github.com/mysql/mysql-server/blob/5.7/sql-common/client.c#L3478
		num, null, off := parseLengthEncodedInt(data[pos:])
		pos += off
		if !null {
			packet.Auth = data[pos : pos+int(num)]
			pos += int(num)
		}
	} else if capability&mysql.ClientSecureConnection > 0 {
		// auth length and auth
		authLen := int(data[pos])
		pos++
		packet.Auth = data[pos : pos+authLen]
		pos += authLen
	} else {
		packet.Auth = data[pos : pos+bytes.IndexByte(data[pos:], 0)]
		pos += len(packet.Auth) + 1
	}

	if capability&mysql.ClientConnectWithDB > 0 {
		if len(data[pos:]) > 0 {
			idx := bytes.IndexByte(data[pos:], 0)
			packet.DBName = string(data[pos : pos+idx])
			pos = pos + idx + 1
		}
	}

	if capability&mysql.ClientPluginAuth > 0 {
		// TODO: Support mysql.ClientPluginAuth, skip it now
		idx := bytes.IndexByte(data[pos:], 0)
		pos = pos + idx + 1
	}

	if capability&mysql.ClientConnectAtts > 0 {
		if len(data[pos:]) == 0 {
			// Defend some ill-formated packet, connection attribute is not important and can be ignored.
			return nil
		}
		if num, null, off := parseLengthEncodedInt(data[pos:]); !null {
			pos += off
			kv := data[pos : pos+int(num)]
			attrs, err := parseAttrs(kv)
			if err != nil {
				return errors.Trace(err)
			}
			packet.Attrs = attrs
			pos += int(num)
		}
	}
	return nil
}

func parseAttrs(data []byte) (map[string]string, error) {
	attrs := make(map[string]string)
	pos := 0
	for pos < len(data) {
		key, _, off, err := parseLengthEncodedBytes(data[pos:])
		if err != nil {
			return attrs, errors.Trace(err)
		}
		pos += off
		value, _, off, err := parseLengthEncodedBytes(data[pos:])
		if err != nil {
			return attrs, errors.Trace(err)
		}
		pos += off

		attrs[string(key)] = string(value)
	}
	return attrs, nil
}

func (cc *clientConn) readHandshakeResponse() error {
	data, err := cc.readPacket()
	if err != nil {
		return errors.Trace(err)
	}

	var p handshakeResponse41
	if err = handshakeResponseFromData(&p, data); err != nil {
		return errors.Trace(err)
	}
	cc.capability = p.Capability & defaultCapability
	cc.user = p.User
	cc.dbname = p.DBName
	cc.collation = p.Collation
	cc.attrs = p.Attrs
	return nil
}

// Run reads client query and writes query result to client in for loop, if there is a panic during query handling,
// it will be recovered and log the panic error.
// This function returns and the connection is closed if there is an IO error or there is a panic.
func (cc *clientConn) Run() {
	defer func() {
		r := recover()
		if r != nil {
			const size = 4096
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			log.Errorf("lastCmd %s, %v, %s", cc.lastCmd, r, buf)
		}
		cc.Close()
	}()

	for {
		cc.alloc.Reset()
		data, err := cc.readPacket()
		if err != nil {
			// if terror.ErrorNotEqual(err, io.EOF) {
			if err != io.EOF {
				log.Error(errors.ErrorStack(err))
			}
			return
		}

		if err := cc.dispatch(data); err != nil {
			if err == io.EOF {
				//if terror.ErrorEqual(err, io.EOF) {
				return
			}
			cmd := string(data[1:])
			log.Warningf("[%d] dispatch error:\n%s\n%s\n%s", cc.connectionID, cc, cmd, errors.ErrorStack(err))
			cc.writeError(err)
		}

		cc.pkt.sequence = 0
	}
}

// dispatch handles client request based on command which is the first byte of the data.
// It also gets a token from server which is used to limit the concurrently handling clients.
// The most frequently used command is ComQuery.
func (cc *clientConn) dispatch(data []byte) error {
	cmd := data[0]
	data = data[1:]
	cc.lastCmd = hack.String(data)

	token := cc.server.getToken()

	startTS := time.Now()
	defer func() {
		cc.server.releaseToken(token)

		log.Infof("[TIME_CMD] %v %d", time.Since(startTS), cmd)
	}()
	log.Infof("dispatch cmd = %v", cmd)

	switch cmd {
	case mysql.ComSleep:
		// TODO: According to mysql document, this command is supposed to be used only internally.
		// So it's just a temp fix, not sure if it's done right.
		// Investigate this command and write test case later.
		return nil
	case mysql.ComQuit:
		return io.EOF
	case mysql.ComQuery: // Most frequently used command.
		// For issue 1989
		// Input payload may end with byte '\0', we didn't find related mysql document about it, but mysql
		// implementation accept that case. So trim the last '\0' here as if the payload an EOF string.
		// See http://dev.mysql.com/doc/internals/en/com-query.html
		if data[len(data)-1] == 0 {
			data = data[:len(data)-1]
		}
		return cc.handleQuery(hack.String(data))
	case mysql.ComPing:
		return cc.writeOK()
	case mysql.ComInitDB:
		log.Infof("init db", hack.String(data))
		if err := cc.useDB(hack.String(data)); err != nil {
			return errors.Trace(err)
		}
		return cc.writeOK()
	case mysql.ComFieldList:
	case mysql.ComStmtPrepare:
	case mysql.ComStmtExecute:
	case mysql.ComStmtClose:
	case mysql.ComStmtSendLongData:
	case mysql.ComStmtReset:
	case mysql.ComSetOption:
	default:
		return mysql.NewErrf(mysql.ErrUnknown, "command %d not supported now", cmd)
	}
	return nil
}

func (cc *clientConn) useDB(db string) (err error) {
	// if input is "use `SELECT`", mysql client just send "SELECT"
	// so we add `` around db.
	// _, err = cc.vtgate.ExecRaw("use `"+db+"`", nil)
	err = cc.vtgate.SetKeyspace(db)
	if err != nil {
		return errors.Trace(err)
	}
	cc.dbname = db
	return
}

func (cc *clientConn) flush() error {
	return cc.pkt.flush()
}

func (cc *clientConn) Status() uint16 {
	return 0
}

func (cc *clientConn) WarningCount() uint16 {
	return 0
}

func (cc *clientConn) AffectedRows() uint64 {
	if cc.lastResult == nil {
		return 0
	} else {
		return cc.lastResult.RowsAffected
	}
}

func (cc *clientConn) LastInsertID() uint64 {
	if cc.lastResult == nil {
		return 0
	} else {
		return cc.lastResult.InsertID
	}
}

func (cc *clientConn) writeOK() error {
	data := cc.alloc.AllocWithLen(4, 32)
	data = append(data, mysql.OKHeader)
	data = append(data, dumpLengthEncodedInt(uint64(cc.AffectedRows()))...)
	data = append(data, dumpLengthEncodedInt(uint64(cc.LastInsertID()))...)
	if cc.capability&mysql.ClientProtocol41 > 0 {
		data = append(data, dumpUint16(cc.Status())...)
		data = append(data, dumpUint16(cc.WarningCount())...)
	}

	err := cc.writePacket(data)
	if err != nil {
		return errors.Trace(err)
	}

	return errors.Trace(cc.flush())
}

func (cc *clientConn) writeError(e error) error {

	m := mysql.NewErrf(mysql.ErrUnknown, e.Error())

	data := cc.alloc.AllocWithLen(4, 16+len(m.Message))
	data = append(data, mysql.ErrHeader)
	data = append(data, byte(m.Code), byte(m.Code>>8))
	if cc.capability&mysql.ClientProtocol41 > 0 {
		data = append(data, '#')
		data = append(data, m.State...)
	}

	data = append(data, m.Message...)

	err := cc.writePacket(data)
	if err != nil {
		return errors.Trace(err)
	}
	return errors.Trace(cc.flush())
}

// writeEOF writes an EOF packet.
// Note this function won't flush the stream because maybe there are more
// packets following it, the "more" argument would indicates that case.
// If "more" is true, a mysql.ServerMoreResultsExists bit would be set
// in the packet.
func (cc *clientConn) writeEOF(more bool) error {
	data := cc.alloc.AllocWithLen(4, 9)

	data = append(data, mysql.EOFHeader)
	if cc.capability&mysql.ClientProtocol41 > 0 {
		data = append(data, dumpUint16(cc.WarningCount())...)
		status := cc.Status()
		if more {
			status |= mysql.ServerMoreResultsExists
		}
		data = append(data, dumpUint16(status)...)
	}

	err := cc.writePacket(data)
	return errors.Trace(err)
}

func (cc *clientConn) writeReq(filePath string) error {
	data := cc.alloc.AllocWithLen(4, 5+len(filePath))
	data = append(data, mysql.LocalInFileHeader)
	data = append(data, filePath...)

	err := cc.writePacket(data)
	if err != nil {
		return errors.Trace(err)
	}

	return errors.Trace(cc.flush())
}

const queryLogMaxLen = 2048

func (cc *clientConn) handleQuery(sql string) (err error) {
	cc.lastResult = nil
	log.Infof("sql = '%v'", sql)
	var vtResult *sqltypes.Result

	if strings.EqualFold(sql, "BEGIN") {
		err = cc.vtgate.Begin()
	} else if strings.EqualFold(sql, "COMMIT") {
		err = cc.vtgate.Commit()
	} else if strings.EqualFold(sql, "ROLLBACK") {
		err = cc.vtgate.Rollback()
	} else if strings.Contains(sql, "autocommit") || strings.Contains(sql, "AUTOCOMMIT") {
		err = cc.vtgate.SetAutoCommit(sql)
	} else {
		vtResult, err = cc.vtgate.Exec(sql, nil)
	}

	if err != nil {
		return errors.Trace(err)
	}
	cc.lastResult = vtResult
	if vtResult == nil || len(vtResult.Rows) == 0 {
		err = cc.writeOK()
	} else {
		err = cc.writeVtResultSet(vtResult, false, false)
	}
	return errors.Trace(err)
}

// handleFieldList returns the field list for a table.
// The sql string is composed of a table name and a terminating character \x00.
func (cc *clientConn) handleFieldList(sql string) (err error) {
	return errors.New("Not Implemented.")
}

func (cc *clientConn) writeVtResultSet(vrs *sqltypes.Result, binary bool, more bool) error {

	fields := vrs.Fields
	rows := vrs.Rows
	columnLen := dumpLengthEncodedInt(uint64(len(fields)))
	data := cc.alloc.AllocWithLen(4, 1024)
	data = append(data, columnLen...)
	err := cc.writePacket(data)
	if err != nil {
		return errors.Trace(err)
	}
	columns := make([]*ColumnInfo, len(fields))
	for i, f := range fields {
		data = data[0:4]
		v := columnInfoFromField(f)
		columns[i] = &v
		log.Infof("ColumnInfo = %v", v)
		data = append(data, v.Dump(cc.alloc)...)
		if err = cc.writePacket(data); err != nil {
			return errors.Trace(err)
		}
	}

	if err = cc.writeEOF(false); err != nil {
		return errors.Trace(err)
	}

	for _, row := range rows {
		if err != nil {
			return errors.Trace(err)
		}
		if row == nil {
			break
		}
		data = data[0:4]
		for _, value := range row {
			if value.IsNull() {
				data = append(data, 0xfb)
				continue
			}
			data = append(data, dumpLengthEncodedString(value.Raw(), cc.alloc)...)
		}

		if err = cc.writePacket(data); err != nil {
			return errors.Trace(err)
		}
	}

	err = cc.writeEOF(more)
	if err != nil {
		return errors.Trace(err)
	}

	return errors.Trace(cc.flush())
}

func columnInfoFromField(f *query.Field) ColumnInfo {
	typ, flg := sqltypes.TypeToMySQL(f.Type)
	c := ColumnInfo{
		//		Schema:             f.Schema,
		Table:        f.Table,
		OrgTable:     f.OrgTable,
		Name:         f.Name,
		OrgName:      f.OrgName,
		ColumnLength: uint32(f.ColumnLength),
		Charset:      uint16(f.Charset),
		Flag:         uint16(flg),
		//		Decimal:            uint8(f.Decimal),
		Type: uint8(typ),
		//		DefaultValueLength: f.DefaultValueLength,
		//		DefaultValue:       f.DefaultValue,
	}
	return c
}
