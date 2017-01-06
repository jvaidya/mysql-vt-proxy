// vtconn.go
package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/youtube/vitess/go/sqltypes"
	topodatapb "github.com/youtube/vitess/go/vt/proto/topodata"
	"github.com/youtube/vitess/go/vt/topo/topoproto"

	"github.com/youtube/vitess/go/vt/vitessdriver"
	"github.com/youtube/vitess/go/vt/vtgate/vtgateconn"
)

type VtConn struct {
	cfg             vitessdriver.Configuration
	conn            *vtgateconn.VTGateConn
	tx              *vtgateconn.VTGateTx
	autocommit      bool
	tabletTypeProto topodatapb.TabletType
}

func NewVtConn(address, keyspace, tabletType string, timeout time.Duration) (*VtConn, error) {
	vtConn := VtConn{}
	vtConn.cfg = vitessdriver.Configuration{}
	vtConn.cfg.Address = address
	vtConn.cfg.Keyspace = keyspace
	vtConn.cfg.TabletType = tabletType
	vtConn.cfg.Timeout = timeout
	vtConn.cfg.Streaming = false
	vtConn.autocommit = false
	var err error
	vtConn.tabletTypeProto, err = topoproto.ParseTabletType(vtConn.cfg.TabletType)
	if err != nil {
		return nil, err
	}
	err = vtConn.dial()
	if err != nil {
		return nil, err
	}
	return &vtConn, err
}

func (v *VtConn) dial() (err error) {
	v.conn, err = vtgateconn.Dial(context.Background(), v.cfg.Address, v.cfg.Timeout, v.cfg.Keyspace)
	return
}

func (v *VtConn) SetKeyspace(keyspace string) error {
	v.cfg.Keyspace = keyspace
	return v.dial()
}

func (v *VtConn) Exec(query string, bindVars map[string]interface{}) (*sqltypes.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), v.cfg.Timeout)
	defer cancel()
	qr, err := v.exec(ctx, query, bindVars)
	if err != nil {
		return nil, err
	}
	return qr, err
}

func (v *VtConn) SetAutoCommit(query string) (err error) {

	if strings.HasSuffix(query, "autocommit=1") || strings.HasSuffix(query, "AUTOCOMMIT=1") {
		v.autocommit = true
	} else if strings.HasSuffix(query, "autocommit=0") || strings.HasSuffix(query, "AUTOCOMMIT=0") {
		v.autocommit = false
	} else {
		err = errors.New("Error setting autocommit.")
	}
	return
}

func (v *VtConn) exec(ctx context.Context, query string, bindVars map[string]interface{}) (*sqltypes.Result, error) {
	if v.tx != nil {
		return v.tx.Execute(ctx, query, bindVars, v.tabletTypeProto, nil)
	}
	// Handle autocommit.
	if v.autocommit {
		err := v.Begin()
		if err != nil {
			return nil, err
		}
		defer func() {
			v.tx = nil
		}()
		r, err := v.tx.Execute(ctx, query, bindVars, v.tabletTypeProto, nil)
		if err != nil {
			return nil, err
		}
		err = v.Commit()
		if err != nil {
			return nil, err
		}
		return r, err
	}
	// Non-transactional case.
	return v.conn.Execute(ctx, query, bindVars, v.tabletTypeProto, nil)
}

func (v *VtConn) Close() error {
	v.conn.Close()
	return nil
}

func (v *VtConn) Begin() error {
	ctx, cancel := context.WithTimeout(context.Background(), v.cfg.Timeout)
	defer cancel()

	tx, err := v.conn.Begin(ctx)
	if err != nil {
		return err
	}
	v.tx = tx
	return nil
}

func (v *VtConn) Commit() error {
	ctx, cancel := context.WithTimeout(context.Background(), v.cfg.Timeout)
	defer cancel()
	return v.CommitContext(ctx)
}

func (v *VtConn) CommitContext(ctx context.Context) error {
	if v.tx == nil {
		return errors.New("commit: not in transaction")
	}
	defer func() {
		v.tx = nil
	}()
	return v.tx.Commit(ctx)
}

func (v *VtConn) Rollback() error {
	ctx, cancel := context.WithTimeout(context.Background(), v.cfg.Timeout)
	defer cancel()
	return v.RollbackContext(ctx)
}

func (v *VtConn) RollbackContext(ctx context.Context) error {
	if v.tx == nil {
		return nil
	}
	defer func() {
		v.tx = nil
	}()
	return v.tx.Rollback(ctx)
}
