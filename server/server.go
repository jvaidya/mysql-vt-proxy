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
	"encoding/json"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	// For pprof
	_ "net/http/pprof"

	log "github.com/golang/glog"
	"github.com/juju/errors"
	"github.com/pingcap/tidb/util/arena"
)

var (
	baseConnID uint32
)

// Server is the MySQL protocol server
type Server struct {
	cfg               *Config
	listener          net.Listener
	rwlock            *sync.RWMutex
	concurrentLimiter *TokenLimiter
	clients           map[uint32]*clientConn
}

// ConnectionCount gets current connection count.
func (s *Server) ConnectionCount() int {
	var cnt int
	s.rwlock.RLock()
	cnt = len(s.clients)
	s.rwlock.RUnlock()
	return cnt
}

func (s *Server) getToken() *Token {
	return s.concurrentLimiter.Get()
}

func (s *Server) releaseToken(token *Token) {
	s.concurrentLimiter.Put(token)
}

// Generate a random string using ASCII characters but avoid separator character.
// See https://github.com/mysql/mysql-server/blob/5.7/mysys_ssl/crypt_genhash_impl.cc#L435
func randomBuf(size int) []byte {
	buf := make([]byte, size)
	for i := 0; i < size; i++ {
		buf[i] = byte(rand.Intn(127))
		if buf[i] == 0 || buf[i] == byte('$') {
			buf[i]++
		}
	}
	return buf
}

// newConn creates a new *clientConn from a net.Conn.
// It allocates a connection ID and random salt data for authentication.
func (s *Server) newConn(conn net.Conn) *clientConn {
	// Connection to vtgate.
	vtgate, err := NewVtConn(s.cfg.VtAddress, s.cfg.VtKeyspace, s.cfg.VtTabletType, 2*time.Second)

	if err != nil {
		log.Errorf("Could not connect to vtgage: %v", err)
	}
	cc := &clientConn{
		conn:         conn,
		pkt:          newPacketIO(conn),
		server:       s,
		connectionID: atomic.AddUint32(&baseConnID, 1),
		alloc:        arena.NewAllocator(32 * 1024),
		vtgate:       vtgate,
	}
	log.Infof("[%d] new connection %s", cc.connectionID, conn.RemoteAddr().String())
	cc.salt = randomBuf(20)
	return cc
}

func (s *Server) skipAuth() bool {
	return s.cfg.SkipAuth
}

const tokenLimit = 1000

// NewServer creates a new Server.
func NewServer(cfg *Config) (*Server, error) {
	s := &Server{
		cfg:               cfg,
		concurrentLimiter: NewTokenLimiter(tokenLimit),
		rwlock:            &sync.RWMutex{},
		clients:           make(map[uint32]*clientConn),
	}

	var err error
	cfg.SkipAuth = true
	if cfg.Socket != "" {
		s.listener, err = net.Listen("unix", cfg.Socket)
	} else {
		s.listener, err = net.Listen("tcp", s.cfg.Addr)
	}
	if err != nil {
		return nil, errors.Trace(err)
	}

	// Init rand seed for randomBuf()
	rand.Seed(time.Now().UTC().UnixNano())
	log.Infof("Server run MySQL Protocol Listen at [%s]", s.cfg.Addr)
	return s, nil
}

// Run runs the server.
func (s *Server) Run() error {

	// Start http api to report stats.
	if s.cfg.ReportStatus {
		s.startStatusHTTP()
	}

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok {
				if opErr.Err.Error() == "use of closed network connection" {
					return nil
				}
			}
			log.Errorf("accept error %s", err.Error())
			return errors.Trace(err)
		}

		go s.onConn(conn)
	}
}

// Close closes the server.
func (s *Server) Close() {
	s.rwlock.Lock()
	defer s.rwlock.Unlock()

	if s.listener != nil {
		s.listener.Close()
		s.listener = nil
	}
}

// onConn runs in its own goroutine, handles queries from this connection.
func (s *Server) onConn(c net.Conn) {
	conn := s.newConn(c)
	if err := conn.handshake(); err != nil {
		// Some keep alive services will send request to TiDB and disconnect immediately.
		// So we use info log level.
		log.Infof("handshake error %s", errors.ErrorStack(err))
		c.Close()
		return
	}
	defer func() {
		log.Infof("[%d] close connection", conn.connectionID)
	}()

	s.rwlock.Lock()
	s.clients[conn.connectionID] = conn
	s.rwlock.Unlock()

	conn.Run()
}

var once sync.Once

const defaultStatusAddr = ":10080"

func (s *Server) startStatusHTTP() {
	once.Do(func() {
		go func() {
			http.HandleFunc("/status", func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				s := status{
					Connections: s.ConnectionCount(),
					Version:     "mysql-vt-proxy v 0.1",
				}
				js, err := json.Marshal(s)
				if err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					log.Error("Encode json error", err)
				} else {
					w.Write(js)
				}

			})
			addr := s.cfg.StatusAddr
			if len(addr) == 0 {
				addr = defaultStatusAddr
			}
			log.Infof("Listening on %v for status and metrics report.", addr)
			err := http.ListenAndServe(addr, nil)
			if err != nil {
				log.Fatal(err)
			}
		}()
	})
}

// TiDB status
type status struct {
	Connections int    `json:"connections"`
	Version     string `json:"version"`
	GitHash     string `json:"git_hash"`
}

// Server error codes.
const (
	codeUnknownFieldType  = 1
	codeInvalidPayloadLen = 2
	codeInvalidSequence   = 3
	codeInvalidType       = 4

	codeNotAllowedCommand = 1148
)
