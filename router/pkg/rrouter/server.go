package rrouter

import (
	"crypto/tls"
	"fmt"
	"reflect"
	"sync"

	"github.com/jackc/pgproto3"
	"github.com/pg-sharding/spqr/pkg/config"
	"github.com/pg-sharding/spqr/pkg/conn"
	"github.com/pg-sharding/spqr/pkg/models/kr"
	"github.com/wal-g/tracelog"
	"golang.org/x/xerrors"
)

type Server interface {
	Send(query pgproto3.FrontendMessage) error
	Receive() (pgproto3.BackendMessage, error)

	AddShard(shkey kr.ShardKey) error
	UnrouteShard(sh kr.ShardKey) error

	AddTLSConf(cfg *tls.Config) error

	Cleanup() error
	Reset() error
}

type ShardServer struct {
	rule *config.BERule

	pool conn.ConnPool

	shard Shard
}

func (srv *ShardServer) Reset() error {
	return nil
}

func (srv *ShardServer) UnrouteShard(shkey kr.ShardKey) error {

	if srv.shard.SHKey().Name != shkey.Name {
		return xerrors.Errorf("active shard does not match unrouted: %v != %v", srv.shard.SHKey().Name, shkey.Name)
	}

	pgi := srv.shard.Instance()
	fmt.Printf("put connection to %v back to pool\n", pgi.Hostname())

	if err := srv.pool.Put(shkey, pgi); err != nil {
		return err
	}

	srv.shard = nil

	return nil
}

func (srv *ShardServer) AddShard(shkey kr.ShardKey) error {
	if srv.shard != nil {
		return xerrors.New("single shard server does not support more than 2 shard connection simultaneously")
	}

	if pgi, err := srv.pool.Connection(shkey); err != nil {
		return err
	} else {

		srv.shard, err = NewShard(shkey, pgi, config.Get().RouterConfig.ShardMapping[shkey.Name])
		if err != nil {
			return err
		}
	}

	return nil
}

func (srv *ShardServer) AddTLSConf(cfg *tls.Config) error {
	return srv.shard.ReqBackendSsl(cfg)
}

func NewShardServer(rule *config.BERule, spool conn.ConnPool) *ShardServer {
	return &ShardServer{
		rule: rule,
		pool: spool,
	}
}

func (srv *ShardServer) Send(query pgproto3.FrontendMessage) error {
	return srv.shard.Send(query)
}

func (srv *ShardServer) Receive() (pgproto3.BackendMessage, error) {
	return srv.shard.Receive()
}

func (srv *ShardServer) Cleanup() error {

	if srv.rule.PoolRollback {
		if err := srv.Send(&pgproto3.Query{
			String: "ROLLBACK",
		}); err != nil {
			return err
		}
	}

	if srv.rule.PoolDiscard {
		if err := srv.Send(&pgproto3.Query{
			String: "DISCARD ALL",
		}); err != nil {
			return err
		}

	}

	return nil
}

var _ Server = &ShardServer{}

type MultiShardServer struct {
	rule         *config.BERule
	activeShards []Shard

	pool conn.ConnPool
}

func (m *MultiShardServer) Reset() error {
	panic("implement me")
}

func (m *MultiShardServer) AddShard(shkey kr.ShardKey) error {
	pgi, err := m.pool.Connection(shkey)
	if err != nil {
		return err
	}

	sh, err := NewShard(shkey, pgi, config.Get().RouterConfig.ShardMapping[shkey.Name])
	if err != nil {
		return err
	}

	m.activeShards = append(m.activeShards, sh)

	return nil
}

func (m *MultiShardServer) UnrouteShard(sh kr.ShardKey) error {

	for _, activeShard := range m.activeShards {
		if activeShard.Name() == sh.Name {
			return nil
		}
	}

	return xerrors.New("unrouted shard does not match any of active")
}

func (m *MultiShardServer) AddTLSConf(cfg *tls.Config) error {
	for _, shard := range m.activeShards {
		_ = shard.ReqBackendSsl(cfg)
	}

	return nil
}

func (m *MultiShardServer) Send(msg pgproto3.FrontendMessage) error {
	for _, shard := range m.activeShards {
		tracelog.InfoLogger.Printf("sending Q to sh %v", shard.Name())
		err := shard.Send(msg)
		if err != nil {
			tracelog.InfoLogger.PrintError(err)
			//
		}
	}

	return nil
}

func (m *MultiShardServer) Receive() (pgproto3.BackendMessage, error) {
	ch := make(chan pgproto3.BackendMessage, len(m.activeShards))

	wg := sync.WaitGroup{}

	wg.Add(len(m.activeShards))
	for _, shard := range m.activeShards {
		tracelog.InfoLogger.Printf("recv mult resp from sh %s", shard.Name())

		go func(shard Shard, ch chan<- pgproto3.BackendMessage, wg *sync.WaitGroup) error {
			defer wg.Done()

			msg, err := shard.Receive()
			if err != nil {
				return err
			}
			tracelog.InfoLogger.Printf("got %v from %s", msg, shard.Name())

			ch <- msg

			return nil
		}(shard, ch, &wg)
	}

	wg.Wait()
	close(ch)

	msgs := make([]pgproto3.BackendMessage, 0, len(m.activeShards))

	for {
		msg, ok := <-ch
		if !ok {
			break
		}
		msgs = append(msgs, msg)
	}

	for i := range msgs {
		if reflect.TypeOf(msgs[0]) != reflect.TypeOf(msgs[i]) {
			return nil, xerrors.Errorf("got messages with different types from multiconnection %T, %T", msgs[0], msgs[i])
		}
	}

	tracelog.InfoLogger.Printf("compute multi server msgs from %T", msgs[0])

	switch v := msgs[0].(type) {
	case *pgproto3.CommandComplete:
		return v, nil
	case *pgproto3.ErrorResponse:
		return v, nil
	case *pgproto3.ReadyForQuery:
		return v, nil
	case *pgproto3.DataRow:
		ret := &pgproto3.DataRow{}

		for i, msg := range msgs {
			if i == 0 {
				ret = msg.(*pgproto3.DataRow)
				continue
			}
			drow := msg.(*pgproto3.DataRow)
			ret.Values = append(ret.Values, drow.Values...)
		}

		return ret, nil

	default:
		return &pgproto3.ErrorResponse{Severity: "ERROR", Message: fmt.Sprintf("failed to conpose responce %T", v)}, nil
	}
}

func (m *MultiShardServer) Cleanup() error {

	if m.rule.PoolRollback {
		if err := m.Send(&pgproto3.Query{
			String: "ROLLBACK",
		}); err != nil {
			return err
		}
	}

	if m.rule.PoolDiscard {
		if err := m.Send(&pgproto3.Query{
			String: "DISCARD ALL",
		}); err != nil {
			return err
		}
	}

	return nil
}

var _ Server = &MultiShardServer{}

func NewMultiShardServer(rule *config.BERule, pool conn.ConnPool) (Server, error) {
	ret := &MultiShardServer{
		rule:         rule,
		pool:         pool,
		activeShards: []Shard{},
	}

	return ret, nil
}

type LoadMirroringServer struct {
	main   Server
	mirror Server
}

func (s LoadMirroringServer) Reset() error {
	panic("implement me")
}

func (s LoadMirroringServer) AddShard(shkey kr.ShardKey) error {
	panic("implement me")
}

func (s LoadMirroringServer) UnrouteShard(sh kr.ShardKey) error {
	panic("implement me")
}

func (s LoadMirroringServer) AddTLSConf(cfg *tls.Config) error {
	panic("implement me")
}

func (s LoadMirroringServer) Cleanup() error {
	panic("implement me")
}

var _ Server = &LoadMirroringServer{}

func NewLoadMirroringServer(s1 Server, s2 Server) *LoadMirroringServer {
	return &LoadMirroringServer{}
}

func (LoadMirroringServer) Send(query pgproto3.FrontendMessage) error {
	return nil
}
func (LoadMirroringServer) Receive() (pgproto3.BackendMessage, error) {
	return nil, nil
}
