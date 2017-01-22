package manager

import (
	"golang.org/x/net/context"

	"github.com/Sirupsen/logrus"
	"github.com/arkbriar/ss-mgr/manager/protocol"
	"google.golang.org/grpc"
)

// ShadowsocksService contains the necessary infos of a shadowsock service.
type ShadowsocksService struct {
	UserId   string `json:"user_id"`
	Port     int32  `json:"server_port"`
	Password string `json:"password"`
}

// slaveMeta represents the meta information required by a local slave object.
type slaveMeta struct {
	openedPorts map[int32]*ShadowsocksService
	stats       map[int32]int64
}

func (m *slaveMeta) addPorts(srvs ...*ShadowsocksService) {
	for _, srv := range srvs {
		m.openedPorts[srv.Port] = srv
		m.stats[srv.Port] = 0
	}
}

func (m *slaveMeta) removePorts(srvs ...*ShadowsocksService) {
	for _, srv := range srvs {
		delete(m.openedPorts, srv.Port)
		delete(m.stats, srv.Port)
	}
}

func (m *slaveMeta) setStats(stats map[int32]int64) {
	for port, traffic := range stats {
		m.stats[port] = traffic
	}
}

func (m *slaveMeta) ListServices() []*ShadowsocksService {
	srvs := make([]*ShadowsocksService, 0, len(m.openedPorts))
	for _, srv := range m.openedPorts {
		srvs = append(srvs, srv)
	}
	return srvs
}

func (m *slaveMeta) GetStats() map[int32]int64 {
	return m.stats
}

// Slave provides interfaces for managing the remote slave.
type Slave interface {
	// Dial opens the connection to the remote slave node.
	Dial() error
	// Close closes the connection.
	Close() error
	// Allocate adds services on the remote slave node.
	// The services added is returned.
	Allocate(srvs ...*ShadowsocksService) ([]*ShadowsocksService, error)
	// Free removes services on the remote slave node.
	// The services removed is returned.
	Free(srvs ...*ShadowsocksService) ([]*ShadowsocksService, error)
	// ListServices gets all alive services.
	ListServices() ([]*ShadowsocksService, error)
	// GetStats gets the traffic statistics of all alive services.
	GetStats() (map[int32]int64, error)
	// SetStats sets the traffic statistics of all alive services.
	SetStats(traffics map[int32]int64) error
	// Meta returns a copy of local meta object of slave.
	Meta() slaveMeta
}

// slave is the true object of remote slave process. It implements the
// `Slave` interface.
type slave struct {
	remoteURL string                                 // remote slave's grpc service url
	conn      *grpc.ClientConn                       // grpc client connection
	stub      protocol.ShadowsocksManagerSlaveClient // remote slave's grpc service client
	token     string                                 // token used to communicate with remote slave
	ctx       context.Context                        // context for grpc communication
	meta      slaveMeta                              // meta store meta information such as services, etc.
	Slave
}

// tokenType is the key type for context
type tokenType string

// NewSlave generates a new slave instance to communicate with.
func NewSlave(url, token string) Slave {
	return &slave{
		remoteURL: url,
		conn:      nil,
		stub:      nil,
		token:     token,
		ctx:       context.WithValue(context.Background(), tokenType("Token"), token),
		meta: slaveMeta{
			openedPorts: make(map[int32]*ShadowsocksService),
		},
	}
}

func (s *slave) isTokenValid() bool {
	return len(s.token) == 0
}

func (s *slave) Dial() error {
	// FIXME(arkbriar@gmail.com) Here I initialize the connection using `grpc.WithInsecure`.
	conn, err := grpc.Dial(s.remoteURL, grpc.WithInsecure())
	if err != nil {
		return err
	}
	s.conn = conn
	s.stub = protocol.NewShadowsocksManagerSlaveClient(conn)
	return nil
}

func (s *slave) Close() error {
	conn := s.conn
	s.conn, s.stub = nil, nil
	return conn.Close()
}

func (s *slave) Allocate(srvs ...*ShadowsocksService) ([]*ShadowsocksService, error) {
	serviceList := constructProtocolServiceList(srvs...)
	resp, err := s.stub.Allocate(s.ctx, &protocol.AllocateRequest{
		ServiceList: serviceList,
	})
	if err != nil {
		return nil, err
	}
	diff := compareLists(serviceList, resp.GetServiceList())
	allocatedList := constructServiceList(resp.GetServiceList())
	s.meta.addPorts(allocatedList...)
	if len(diff) != 0 {
		return allocatedList, constructErrorFromDifferenceServiceList(diff)
	}
	return allocatedList, nil
}

func (s *slave) Free(srvs ...*ShadowsocksService) ([]*ShadowsocksService, error) {
	serviceList := constructProtocolServiceList(srvs...)
	resp, err := s.stub.Free(s.ctx, &protocol.FreeRequest{
		ServiceList: serviceList,
	})
	if err != nil {
		return nil, err
	}
	diff := compareLists(serviceList, resp.GetServiceList())
	freedList := constructServiceList(resp.GetServiceList())
	s.meta.removePorts(freedList...)
	if len(diff) != 0 {
		return freedList, constructErrorFromDifferenceServiceList(diff)
	}
	return freedList, nil
}

func (s *slave) ListServices() ([]*ShadowsocksService, error) {
	resp, err := s.stub.ListServices(s.ctx, nil)
	if err != nil {
		return nil, err
	}
	// Compare the returned list with those recorded.
	diff := compareLists(constructProtocolServiceList(s.meta.ListServices()...), resp)
	if len(diff) != 0 {
		logrus.Warnln(constructErrorFromDifferenceServiceList(diff))
	}
	return constructServiceList(resp), nil
}

func (s *slave) GetStats() (map[int32]int64, error) {
	resp, err := s.stub.GetStats(s.ctx, nil)
	if err != nil {
		return nil, err
	}
	s.meta.setStats(resp.GetTraffics())
	return resp.GetTraffics(), nil
}

func (s *slave) SetStats(traffics map[int32]int64) error {
	_, err := s.stub.SetStats(s.ctx, &protocol.Statistics{
		Traffics: traffics,
	})
	if err != nil {
		return err
	}
	s.meta.setStats(traffics)
	return nil
}

func (s *slave) Meta() slaveMeta {
	return s.meta
}
