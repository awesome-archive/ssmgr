package shadowsocks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/arkbriar/ss-mgr/slave/shadowsocks/process"
)

type serverOptions struct {
	MPTCP          bool
	TCPFastOpen    bool
	Auth           bool
	NameServer     string
	PidFile        string
	ManagerAddress string
	Interface      string
	FireWall       bool
	Verbose        bool
}

func (o *serverOptions) BuildArgs() []string {
	opts := make([]string, 0)
	if o.MPTCP {
		opts = append(opts, "--mptcp")
	}
	if o.TCPFastOpen {
		opts = append(opts, "--fast-open")
	}
	if o.Auth {
		opts = append(opts, "-A")
	}
	if len(o.NameServer) != 0 {
		opts = append(opts, "-d", o.NameServer)
	}
	// DO NOT USE THIS OPTION
	// When use pid file, ss-server will create a child process and we can
	// not operate on it directly.
	/* if len(o.PidFile) != 0 {
	 *     opts = append(opts, "-f", o.PidFile)
	 * } */
	if len(o.ManagerAddress) != 0 {
		opts = append(opts, "--manager-address", o.ManagerAddress)
	}
	if o.FireWall {
		opts = append(opts, "--firewall")
	}
	if o.Verbose {
		opts = append(opts, "-v")
	}
	return opts
}

var (
	methods = []string{
		"table", "rc4", "rc4-md5", "aes-128-cfb", "aes-192-cfb", "aes-256-cfb",
		"aes-128-ctr", "aes-192-ctr", "aes-256-ctr", "bf-cfb", "camellia-128-cfb",
		"camellia-192-cfb", "camellia-256-cfb", "cast5-cfb", "des-cfb", "idea-cfb",
		"rc2-cfb", "seed-cfb", "salsa20", "chacha20", "chacha20-ietf",
	}
)

// ValidateEncryptMethod validates if the encrypt method is supported.
func ValidateEncryptMethod(m string) bool {
	for _, method := range methods {
		if m == method {
			return true
		}
	}
	return false
}

// Errors of `Manager`
var (
	ErrServerNotFound = errors.New("Server not found.")
	ErrInvalidServer  = errors.New("Invalid server.")
	ErrServerExists   = errors.New("Server already exists.")
)

// Server is a struct describes a shadowsocks server.
type Server struct {
	Host     string `json:"server"`
	Port     int32  `json:"server_port"`
	Password string `json:"password"`
	Method   string `json:"method"`
	Timeout  int    `json:"timeout"`
	stat     atomic.Value
	options  serverOptions
	runtime  struct {
		path   string
		cmd    *exec.Cmd
		logw   io.WriteCloser
		config string
	}
}

// Valid checks if it is a valid server configuration.
func (s *Server) Valid() bool {
	return len(s.Host) != 0 && s.Port > 0 && s.Port < 65536 && len(s.Password) >= 8 && ValidateEncryptMethod(s.Method) && s.Timeout > 0
}

// Save saves this server's configuration to file in JSON.
func (s *Server) Save(filename string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(filename, data, 0644)
	if err != nil {
		return err
	}
	return nil
}

// SavePidFile saves current pid to a file (s.options.PidFile). This method
// is to replace ss-server's '-f' option.
func (s *Server) SavePidFile() error {
	proc := s.Process()
	if len(s.options.PidFile) != 0 && proc != nil {
		return ioutil.WriteFile(s.options.PidFile, []byte(fmt.Sprint(proc.Pid)), 0644)
	}
	return nil
}

func (s *Server) opts() []string {
	var opts []string
	if len(s.runtime.config) != 0 {
		opts = []string{"-c", s.runtime.config}
	} else {
		opts = []string{"-s", s.Host, "-p", fmt.Sprint(s.Port), "-m", s.Method, "-k", s.Password, "-d", fmt.Sprint(s.Timeout)}
	}
	opts = append(opts, s.options.BuildArgs()...)
	return opts
}

// Command constructs a new shadowsock server command
func (s *Server) Command() *exec.Cmd {
	return exec.Command("ss-server", s.opts()...)
}

// String returns the command line string
func (s *Server) String() string {
	return fmt.Sprintf("ss-server %s", strings.Join(s.opts(), " "))
}

func (s *Server) clone() *Server {
	copy := *s
	copy.stat.Store(s.GetStat())
	copy.runtime.logw = nil
	return &copy
}

// GetStat returns the statistics of this server
func (s *Server) GetStat() Stat {
	stat := s.stat.Load()
	if stat == nil {
		return Stat{}
	}
	return stat.(Stat)
}

// Process returns the running process / nil of server
func (s *Server) Process() *os.Process {
	if s.runtime.cmd != nil {
		return s.runtime.cmd.Process
	}
	return nil
}

// Alive returns if the server is alive
func (s *Server) Alive() bool {
	proc := s.Process()
	return proc != nil && process.Alive(proc.Pid)
}

// Stat represents the statistics collected from a shadowsocks server
type Stat struct {
	Traffic int64 `json:"traffic"` // Transfered traffic in bytes
	/* Rx      int64 `json:"rx"`      // Receive in bytes
	 * Tx      int64 `json:"tx"`      // Transmit in bytes */
}

// Manager is an interface provides a few methods to manager shadowsocks
// servers.
type Manager interface {
	// Listen listens udp connection on 127.0.0.1:{udpPort} and handles the stats update
	// sent from ss-server.
	Listen() error
	// Add adds a ss-server with given arguments.
	Add(s *Server) error
	// Remove kills the ss-server if found.
	Remove(port int32) error
	// ListServers list the active ss-servers.
	ListServers() map[int32]*Server
	// GetServer gets a clone of `Server` struct of given port.
	GetServer(port int32) (*Server, error)
}

// Implementation of `Manager` interface.
type manager struct {
	serverLock sync.RWMutex
	servers    map[int32]*Server
	path       string
	udpPort    int
	execLock   sync.RWMutex
}

// NewManager returns a new manager.
func NewManager(udpPort int) Manager {
	return &manager{
		servers: make(map[int32]*Server),
		path:    path.Join(os.Getenv("HOME"), ".shadowsocks_manager"),
		udpPort: udpPort,
	}
}

func (mgr *manager) StatRecvHandler(data []byte) {
	cmd := string(data[:4])
	if string(data[:4]) != "stat" {
		log.Warnf("Unrecognized command %s, dropped", cmd)
		return
	}
	body := bytes.TrimSpace(data[5:])
	log.Debugln("Stat body is", string(body))
	var stat map[string]int64
	err := json.Unmarshal(body, &stat)
	if err != nil {
		log.Warnln("Unmarshal error:", err)
		return
	}
	port, traffic := -1, int64(-1)
	for portS, trafficS := range stat {
		port, _ = strconv.Atoi(portS)
		traffic = trafficS
		break
	}
	if port < 0 || traffic < 0 {
		log.Warnf("Invalid stat!")
		return
	}
	// Update statistic
	mgr.serverLock.RLock()
	defer mgr.serverLock.RUnlock()
	s, ok := mgr.servers[int32(port)]
	if !ok {
		log.Warnf("Server on port %d not found!", port)
		return
	}
	s.stat.Store(Stat{Traffic: traffic})
}

func (mgr *manager) Listen() error {
	port := mgr.udpPort
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	go func() {
		defer conn.Close()
		buf := make([]byte, 1024)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			// the n-th is \x00 to indicate end
			log.Debugf("Receving packet from %s: %s", from, buf[:n-1])
			if err != nil {
				log.Warnln(err)
				continue
			}
			mgr.StatRecvHandler(buf[:n-1])
		}
	}()
	log.Infof("Listening on 127.0.0.1:%d ...", port)
	return nil
}

func (mgr *manager) prepareExec(s *Server) error {
	pathPrefix := path.Join(mgr.path, fmt.Sprint(s.Port))

	s.options.PidFile = path.Join(pathPrefix, "ss_server.pid")
	s.options.ManagerAddress = fmt.Sprintf("127.0.0.1:%d", mgr.udpPort)
	s.options.Verbose = true

	err := os.MkdirAll(pathPrefix, 0744)
	if err != nil {
		return err
	}
	configFile := path.Join(pathPrefix, "ss_server.json")
	err = s.Save(configFile)
	if err != nil {
		return err
	}
	s.runtime.path = pathPrefix
	s.runtime.config = configFile
	return nil
}

func (mgr *manager) deleteResidue(s *Server) error {
	err := os.RemoveAll(s.runtime.path)
	if err != nil {
		log.Warnf("Can not delete managed server path %s", s.runtime.path)
	}
	return err
}

func (mgr *manager) exec(s *Server) error {
	err := mgr.prepareExec(s)
	if err != nil {
		return err
	}
	logw, err := os.Create(path.Join(s.runtime.path, "ss_server.log"))
	if err != nil {
		return err
	}
	cmd := s.Command()
	cmd.Stdout, cmd.Stderr = logw, logw
	s.runtime.logw = logw
	s.runtime.cmd = cmd
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := s.SavePidFile(); err != nil {
		log.Warnf("Can not save pid file, %s", err)
	}
	log.Infof("ss-server running at process %d", cmd.Process.Pid)
	return nil
}

func (mgr *manager) kill(s *Server) {
	if err := s.Process().Kill(); err != nil {
		log.Warnln(err)
	}
	// release process's resource
	s.runtime.cmd.Wait()
	s.runtime.logw.Close()
	mgr.deleteResidue(s)
}

func (mgr *manager) Add(s *Server) error {
	mgr.serverLock.Lock()
	defer mgr.serverLock.Unlock()
	mgr.execLock.Lock()
	defer mgr.execLock.Unlock()
	if _, ok := mgr.servers[s.Port]; ok {
		return ErrServerExists
	}
	if !s.Valid() {
		return ErrInvalidServer
	}
	err := mgr.exec(s)
	if err != nil {
		return err
	}
	mgr.servers[s.Port] = s
	return nil
}

func (mgr *manager) remove(port int32) {
	mgr.serverLock.Lock()
	defer mgr.serverLock.Unlock()
	delete(mgr.servers, port)
}

func (mgr *manager) Remove(port int32) error {
	mgr.serverLock.Lock()
	defer mgr.serverLock.Unlock()
	mgr.execLock.Lock()
	defer mgr.execLock.Unlock()
	s, ok := mgr.servers[port]
	if !ok {
		return ErrServerNotFound
	}
	delete(mgr.servers, port)
	mgr.kill(s)
	return nil
}

func (mgr *manager) ListServers() map[int32]*Server {
	mgr.serverLock.RLock()
	defer mgr.serverLock.RUnlock()
	currentServers := make(map[int32]*Server)
	for port, s := range mgr.servers {
		currentServers[port] = s.clone()
	}
	return currentServers
}

func (mgr *manager) GetServer(port int32) (*Server, error) {
	mgr.serverLock.RLock()
	defer mgr.serverLock.RUnlock()
	s, ok := mgr.servers[port]
	if !ok {
		return nil, ErrServerNotFound
	}
	return s.clone(), nil
}

// ServerMonitor provide a way to monitor all server processes
func (mgr *manager) ServerMonitor() {
	for {
		time.Sleep(5 * time.Second)
		for _, s := range mgr.ListServers() {
			if !s.Alive() {
				mgr.execLock.Lock()
				if err := mgr.exec(s); err != nil {
					log.Warn("Can not restart server", s, ", error is", err)
					log.Warn("Deleting server...")
					mgr.deleteResidue(s)
					mgr.remove(s.Port)
				}
				mgr.execLock.Unlock()
			}
		}
	}
}
