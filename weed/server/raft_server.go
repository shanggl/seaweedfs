package weed_server

import (
	"encoding/json"
	"io/ioutil"
	"math/rand"
	"os"
	"path"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/chrislusf/raft"
	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/topology"
	"github.com/gorilla/mux"
)

type RaftServer struct {
	peers      []string // initial peers to join with
	raftServer raft.Server
	dataDir    string
	httpAddr   string
	router     *mux.Router
	topo       *topology.Topology
}

func NewRaftServer(r *mux.Router, peers []string, httpAddr string, dataDir string, topo *topology.Topology, pulseSeconds int) *RaftServer {
	s := &RaftServer{
		peers:    peers,
		httpAddr: httpAddr,
		dataDir:  dataDir,
		router:   r,
		topo:     topo,
	}

	if glog.V(4) {
		raft.SetLogLevel(2)
	}

	raft.RegisterCommand(&topology.MaxVolumeIdCommand{})

	var err error
	transporter := raft.NewHTTPTransporter("/cluster", 0)
	transporter.Transport.MaxIdleConnsPerHost = 1024
	glog.V(0).Infof("Starting RaftServer with %v", httpAddr)

	// Clear old cluster configurations if peers are changed
	if oldPeers, changed := isPeersChanged(s.dataDir, httpAddr, s.peers); changed {
		glog.V(0).Infof("Peers Change: %v => %v", oldPeers, s.peers)
		os.RemoveAll(path.Join(s.dataDir, "conf"))
		os.RemoveAll(path.Join(s.dataDir, "log"))
		os.RemoveAll(path.Join(s.dataDir, "snapshot"))
	}

	s.raftServer, err = raft.NewServer(s.httpAddr, s.dataDir, transporter, nil, topo, "")
	if err != nil {
		glog.V(0).Infoln(err)
		return nil
	}
	transporter.Install(s.raftServer, s)
	s.raftServer.SetHeartbeatInterval(500 * time.Millisecond)
	s.raftServer.SetElectionTimeout(time.Duration(pulseSeconds) * 500 * time.Millisecond)
	s.raftServer.Start()

	s.router.HandleFunc("/cluster/status", s.statusHandler).Methods("GET")

	for _, peer := range s.peers {
		s.raftServer.AddPeer(peer, "http://"+peer)
	}
	time.Sleep(time.Duration(1000+rand.Int31n(3000)) * time.Millisecond)
	if s.raftServer.IsLogEmpty() {
		// Initialize the server by joining itself.
		glog.V(0).Infoln("Initializing new cluster")

		_, err := s.raftServer.Do(&raft.DefaultJoinCommand{
			Name:             s.raftServer.Name(),
			ConnectionString: "http://" + s.httpAddr,
		})

		if err != nil {
			glog.V(0).Infoln(err)
			return nil
		}
	}

	glog.V(0).Infof("current cluster leader: %v", s.raftServer.Leader())

	return s
}

func (s *RaftServer) Peers() (members []string) {
	peers := s.raftServer.Peers()

	for _, p := range peers {
		members = append(members, strings.TrimPrefix(p.ConnectionString, "http://"))
	}

	return
}

func isPeersChanged(dir string, self string, peers []string) (oldPeers []string, changed bool) {
	confPath := path.Join(dir, "conf")
	// open conf file
	b, err := ioutil.ReadFile(confPath)
	if err != nil {
		return oldPeers, true
	}
	conf := &raft.Config{}
	if err = json.Unmarshal(b, conf); err != nil {
		return oldPeers, true
	}

	for _, p := range conf.Peers {
		oldPeers = append(oldPeers, strings.TrimPrefix(p.ConnectionString, "http://"))
	}
	oldPeers = append(oldPeers, self)

	if len(peers) == 0 && len(oldPeers) <= 1 {
		return oldPeers, false
	}

	sort.Strings(peers)
	sort.Strings(oldPeers)

	return oldPeers, !reflect.DeepEqual(peers, oldPeers)

}

/*
	目前存在的问题：
	1.如果本地存在raft 配置信息，然后远程其他master重启的时候，无法快速加入
	2.如果在启动的时候 没有使用-peers ，那么直接加载原始拓扑，会自动连之前的master，造成困惑。
*/