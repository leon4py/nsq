package consistence

import (
	"errors"
	"github.com/absolute8511/nsq/internal/levellogger"
	"github.com/cenkalti/backoff"
	"net"
	"strconv"
	"sync"
	"time"
)

var (
	ErrAlreadyExist             = errors.New("already exist")
	ErrTopicNotCreated          = errors.New("topic is not created")
	ErrSessionNotExist          = errors.New("session not exist")
	ErrNotNsqLookupLeader       = errors.New("Not nsqlookup leader")
	ErrLeaderNodeLost           = NewCoordErr("leader node is lost", CoordTmpErr)
	ErrNodeNotFound             = NewCoordErr("node not found", CoordCommonErr)
	ErrLeaderElectionFail       = NewCoordErr("Leader election failed.", CoordElectionTmpErr)
	ErrNoLeaderCanBeElected     = NewCoordErr("No leader can be elected", CoordElectionTmpErr)
	ErrNodeUnavailable          = NewCoordErr("No node is available for topic", CoordTmpErr)
	ErrJoinISRInvalid           = NewCoordErr("Join ISR failed", CoordCommonErr)
	ErrJoinISRTimeout           = NewCoordErr("Join ISR timeout", CoordCommonErr)
	ErrWaitingJoinISR           = NewCoordErr("The topic is waiting node to join isr", CoordCommonErr)
	ErrLeaderSessionNotReleased = NewCoordErr("The topic leader session is not released", CoordElectionTmpErr)
)

const (
	waitMigrateInterval = time.Minute * 10
)

type JoinISRState struct {
	sync.Mutex
	waitingJoin      bool
	waitingSession   string
	waitingStart     time.Time
	readyNodes       map[string]struct{}
	doneChan         chan struct{}
	isLeadershipWait bool
}

type RpcFailedInfo struct {
	nodeID    string
	topic     string
	partition int
	failTime  time.Time
}

func getOthersExceptLeader(topicInfo *TopicPartionMetaInfo) []string {
	others := make([]string, 0, len(topicInfo.ISR)+len(topicInfo.CatchupList)-1)
	for _, n := range topicInfo.ISR {
		if n == topicInfo.Leader {
			continue
		}
		others = append(others, n)
	}
	others = append(others, topicInfo.CatchupList...)
	return others
}

type NsqLookupCoordinator struct {
	clusterKey         string
	myNode             NsqLookupdNodeInfo
	leaderNode         NsqLookupdNodeInfo
	leadership         NSQLookupdLeadership
	nodesMutex         sync.RWMutex
	nsqdNodes          map[string]NsqdNodeInfo
	rpcMutex           sync.RWMutex
	nsqdRpcClients     map[string]*NsqdRpcClient
	nsqdNodeFailChan   chan struct{}
	stopChan           chan struct{}
	joinStateMutex     sync.Mutex
	joinISRState       map[string]*JoinISRState
	failedRpcMutex     sync.Mutex
	failedRpcList      []RpcFailedInfo
	nsqlookupRpcServer *NsqLookupCoordRpcServer
	wg                 sync.WaitGroup
	nsqdMonitorChan    chan struct{}
}

func SetCoordLogger(log levellogger.Logger, level int32) {
	coordLog.logger = log
	coordLog.level = level
}

func NewNsqLookupCoordinator(cluster string, n *NsqLookupdNodeInfo) *NsqLookupCoordinator {
	coord := &NsqLookupCoordinator{
		clusterKey:       cluster,
		myNode:           *n,
		leadership:       nil,
		nsqdNodes:        make(map[string]NsqdNodeInfo),
		nsqdRpcClients:   make(map[string]*NsqdRpcClient),
		nsqdNodeFailChan: make(chan struct{}, 1),
		stopChan:         make(chan struct{}),
		joinISRState:     make(map[string]*JoinISRState),
		failedRpcList:    make([]RpcFailedInfo, 0),
		nsqdMonitorChan:  make(chan struct{}),
	}
	coord.nsqlookupRpcServer = NewNsqLookupCoordRpcServer(coord)
	return coord
}

func (self *NsqLookupCoordinator) SetLeadershipMgr(l NSQLookupdLeadership) {
	self.leadership = l
}

func RetryWithTimeout(fn func() error) error {
	bo := backoff.NewExponentialBackOff()
	bo.MaxElapsedTime = time.Second * 30
	bo.MaxInterval = time.Second * 5
	return backoff.Retry(fn, bo)
}

// init and register to leader server
func (self *NsqLookupCoordinator) Start() error {
	if self.leadership != nil {
		self.leadership.InitClusterID(self.clusterKey)
		err := self.leadership.Register(&self.myNode)
		if err != nil {
			coordLog.Warningf("failed to register nsqlookup coordinator: %v", err)
			return err
		}
	}
	self.wg.Add(1)
	go self.handleLeadership()
	go self.nsqlookupRpcServer.start(self.myNode.NodeIp, self.myNode.RpcPort)
	return nil
}

func (self *NsqLookupCoordinator) Stop() {
	self.leadership.Unregister()
	self.leadership.Stop()
	close(self.stopChan)
	self.nsqlookupRpcServer.stop()
	self.wg.Wait()
}

func (self *NsqLookupCoordinator) handleLeadership() {
	lookupdLeaderChan := make(chan *NsqLookupdNodeInfo)
	if self.leadership != nil {
		go self.leadership.AcquireAndWatchLeader(lookupdLeaderChan, self.stopChan)
	}
	defer self.wg.Done()
	defer close(self.nsqdMonitorChan)
	for {
		select {
		case l, ok := <-lookupdLeaderChan:
			if !ok {
				coordLog.Warningf("leader chan closed.")
				return
			}
			if l == nil {
				coordLog.Warningln("leader is lost.")
				continue
			}
			if l.GetID() != self.leaderNode.GetID() ||
				l.Epoch != self.leaderNode.Epoch {
				coordLog.Infof("lookup leader changed from %v to %v", self.leaderNode, *l)
				self.leaderNode = *l
				if self.leaderNode.GetID() != self.myNode.GetID() {
					// remove watchers.
					close(self.nsqdMonitorChan)
					self.nsqdMonitorChan = make(chan struct{})
				}
				self.notifyLeaderChanged(self.nsqdMonitorChan)
			}
			if self.leaderNode.GetID() == "" {
				coordLog.Warningln("leader is missing.")
			}
		}
	}
}

func (self *NsqLookupCoordinator) notifyLeaderChanged(monitorChan chan struct{}) {
	if self.leaderNode.GetID() != self.myNode.GetID() {
		coordLog.Infof("I am slave.")
		return
	}
	coordLog.Infof("I am master now.")
	// reload topic information
	if self.leadership != nil {
		newTopics, err := self.leadership.ScanTopics()
		if err != nil {
			coordLog.Errorf("load topic info failed: %v", err)
		} else {
			coordLog.Infof("topic loaded : %v", len(newTopics))
			self.NotifyTopicsToAllNsqdForReload(newTopics)
		}
		for _, t := range newTopics {
			self.wg.Add(1)
			go func() {
				defer self.wg.Done()
				self.watchTopicLeaderSession(monitorChan, t.Name, t.Partition)
			}()
		}
	}

	self.wg.Add(1)
	go func() {
		defer self.wg.Done()
		self.handleNsqdNodes(monitorChan)
	}()
	self.wg.Add(1)
	go func() {
		defer self.wg.Done()
		self.checkTopics(monitorChan)
	}()
	self.wg.Add(1)
	go func() {
		defer self.wg.Done()
		self.rpcFailRetryFunc(monitorChan)
	}()
	self.wg.Add(1)
	go func() {
		defer self.wg.Done()
		self.balanceTopicData(monitorChan)
	}()
}

// for the nsqd node that temporally lost, we need send the related topics to
// it .
func (self *NsqLookupCoordinator) NotifyTopicsToSingleNsqdForReload(topics []*TopicPartionMetaInfo, nodeID string) {
	for _, v := range topics {
		if FindSlice(v.ISR, nodeID) != -1 || FindSlice(v.CatchupList, nodeID) != -1 {
			self.notifySingleNsqdForTopicReload(v, nodeID)
		}
	}
}

func (self *NsqLookupCoordinator) NotifyTopicsToAllNsqdForReload(topics []*TopicPartionMetaInfo) {
	for _, v := range topics {
		self.notifyAllNsqdsForTopicReload(v)
	}
}

func (self *NsqLookupCoordinator) handleNsqdNodes(monitorChan chan struct{}) {
	nsqdNodesChan := make(chan []*NsqdNodeInfo)
	if self.leadership != nil {
		go self.leadership.WatchNsqdNodes(nsqdNodesChan, monitorChan)
	}
	defer func() {
		coordLog.Infof("stop watch the nsqd nodes.")
	}()
	for {
		select {
		case nodes, ok := <-nsqdNodesChan:
			if !ok {
				return
			}
			// check if any nsqd node changed.
			coordLog.Infof("Current nsqd nodes: %v", len(nodes))
			oldNodes := self.nsqdNodes
			newNodes := make(map[string]NsqdNodeInfo)
			for _, v := range nodes {
				coordLog.Infof("nsqd node %v : %v", v.GetID(), v)
				newNodes[v.GetID()] = *v
			}
			self.nodesMutex.Lock()
			self.nsqdNodes = newNodes
			self.nodesMutex.Unlock()
			check := false
			for oldID, oldNode := range oldNodes {
				if _, ok := newNodes[oldID]; !ok {
					coordLog.Warningf("nsqd node failed: %v, %v", oldID, oldNode)
					// if node is missing we need check election immediately.
					check = true
				}
			}

			if self.leadership == nil {
				continue
			}
			topics, scanErr := self.leadership.ScanTopics()
			if scanErr != nil {
				coordLog.Infof("scan topics failed: %v", scanErr)
			}
			for newID, newNode := range newNodes {
				if _, ok := oldNodes[newID]; !ok {
					coordLog.Infof("new nsqd node joined: %v, %v", newID, newNode)
					// notify the nsqd node to recheck topic info.(for
					// temp lost)
					if scanErr == nil {
						self.NotifyTopicsToSingleNsqdForReload(topics, newID)
					}
				}
			}
			if check {
				self.triggerCheckTopics(time.Millisecond)
			}
		}
	}
}

func (self *NsqLookupCoordinator) watchTopicLeaderSession(monitorChan chan struct{}, name string, pid int) {
	leaderChan := make(chan *TopicLeaderSession, 1)
	// close monitor channel should cause the leaderChan closed, so we can quit normally
	if self.leadership != nil {
		go self.leadership.WatchTopicLeader(name, pid, leaderChan, monitorChan)
	}
	defer func() {
		coordLog.Infof("stop watch the topic leader session.")
	}()

	coordLog.Infof("begin watching the topic %v-%v leader session", name, pid)
	for {
		select {
		case n, ok := <-leaderChan:
			if !ok {
				return
			}
			if n == nil || n.LeaderNode == nil {
				// try do election for topic
				self.triggerCheckTopics(time.Millisecond)
				coordLog.Warningf("topic leader is missing: %v-%v", name, pid)
			} else {
				coordLog.Infof("topic leader session changed : %v, %v", n, n.LeaderNode)
				go self.revokeEnableTopicWrite(name, pid, true)
			}
		}
	}
}

func (self *NsqLookupCoordinator) triggerCheckTopics(delay time.Duration) {
	time.Sleep(delay)
	select {
	case self.nsqdNodeFailChan <- struct{}{}:
	default:
	}
}

// check if partition is enough,
// check if replication is enough
// check any unexpected state.
func (self *NsqLookupCoordinator) checkTopics(monitorChan chan struct{}) {
	ticker := time.NewTicker(time.Second * 10)
	waitingMigrateTopic := make(map[string]map[int]time.Time)
	defer func() {
		ticker.Stop()
		coordLog.Infof("check topics quit.")
	}()

	for {
		select {
		case <-monitorChan:
			return
		case <-ticker.C:
			self.doCheckTopics(waitingMigrateTopic)
		case <-self.nsqdNodeFailChan:
			self.doCheckTopics(waitingMigrateTopic)
		}
	}
}

func (self *NsqLookupCoordinator) doCheckTopics(waitingMigrateTopic map[string]map[int]time.Time) {
	coordLog.Infof("do check topics...")
	if self.leadership == nil {
		return
	}
	topics, commonErr := self.leadership.ScanTopics()
	if commonErr != nil {
		coordLog.Infof("scan topics failed. %v", commonErr)
		return
	}
	// TODO: check partition number for topic, maybe failed to create
	// some partition when creating topic.
	self.nodesMutex.RLock()
	currentNodes := self.nsqdNodes
	self.nodesMutex.RUnlock()
	for _, t := range topics {
		needMigrate := false
		if len(t.ISR) < t.Replica {
			coordLog.Infof("ISR is not enough for topic %v, isr is :%v", t.GetTopicDesp(), t.ISR)
			needMigrate = true
		}
		if _, ok := currentNodes[t.Leader]; !ok {
			needMigrate = true
			coordLog.Warningf("topic %v leader %v is lost.", t.GetTopicDesp(), t.Leader)
			coordErr := self.handleTopicLeaderElection(t, currentNodes)
			if coordErr != nil {
				coordLog.Warningf("topic leader election failed: %v", coordErr)
				continue
			}
		} else {
			// check topic leader session key.
			leaderSession, err := self.leadership.GetTopicLeaderSession(t.Name, t.Partition)
			if err != nil {
				coordLog.Infof("topic %v leader session not found.", t.GetTopicDesp())
				// notify the nsqd node to acquire the leader session.
				self.notifyISRTopicMetaInfo(t)
				continue
			}
			if leaderSession.LeaderNode == nil || leaderSession.Session == "" {
				coordLog.Infof("topic %v leader session node is missing.", t.GetTopicDesp())
				self.notifyISRTopicMetaInfo(t)
				continue
			}
		}
		aliveCount := 0
		failedNodes := make([]string, 0)
		for _, replica := range t.ISR {
			if _, ok := currentNodes[replica]; !ok {
				coordLog.Warningf("topic %v isr node %v is lost.", t.GetTopicDesp(), replica)
				needMigrate = true
				failedNodes = append(failedNodes, replica)
			} else {
				aliveCount++
			}
		}
		// handle remove this node from ISR
		self.handleRemoveFailedISRNodes(failedNodes, t)

		self.joinStateMutex.Lock()
		state, ok := self.joinISRState[t.GetTopicDesp()]
		self.joinStateMutex.Unlock()
		state.Lock()
		if ok && state.waitingJoin {
			state.Unlock()
			continue
		}
		state.Unlock()

		partitions, ok := waitingMigrateTopic[t.Name]
		if !ok {
			partitions = make(map[int]time.Time)
			waitingMigrateTopic[t.Name] = partitions
		}

		if needMigrate {
			if _, ok := partitions[t.Partition]; !ok {
				partitions[t.Partition] = time.Now()
			}
			if (aliveCount <= t.Replica/2) ||
				partitions[t.Partition].Before(time.Now().Add(-1*waitMigrateInterval)) {
				coordLog.Infof("begin migrate the topic :%v", t.GetTopicDesp())
				self.handleTopicMigrate(t, currentNodes)
				delete(partitions, t.Partition)
			}
		} else {
			delete(partitions, t.Partition)
		}
		// check if the topic write disabled, and try enable if possible. There is a chance to
		// notify the topic enable write with failure, which may cause the write state is not ok.
		state.Lock()
		if ok && state.waitingJoin {
			// no need check if write disabled
		} else if self.isTopicWriteDisabled(t) {
			coordLog.Infof("the topic write is disabled but not in waiting join state: %v", t)
			go self.revokeEnableTopicWrite(t.Name, t.Partition, true)
		}
		state.Unlock()
	}
}

func (self *NsqLookupCoordinator) isTopicWriteDisabled(topicInfo *TopicPartionMetaInfo) bool {
	c, err := self.acquireRpcClient(topicInfo.Leader)
	if err != nil {
		return false
	}
	return c.IsTopicWriteDisabled(topicInfo)
}

func (self *NsqLookupCoordinator) handleTopicLeaderElection(topicInfo *TopicPartionMetaInfo, currentNodes map[string]NsqdNodeInfo) *CoordErr {
	err := self.waitOldLeaderRelease(topicInfo)
	if err != nil {
		coordLog.Infof("Leader is not released: %v", topicInfo)
		return ErrLeaderSessionNotReleased
	}

	_, _, state, coordErr := self.prepareJoinState(topicInfo.Name, topicInfo.Partition)
	if coordErr != nil {
		coordLog.Infof("prepare join state failed: %v", coordErr)
		return coordErr
	}
	state.Lock()
	defer state.Unlock()
	if state.waitingJoin {
		coordLog.Warningf("failed because another is waiting join.")
		return ErrLeavingISRWait
	}
	if state.doneChan != nil {
		close(state.doneChan)
		state.doneChan = nil
	}
	state.waitingJoin = false
	state.waitingSession = ""

	coordErr = self.notifyISRDisableTopicWrite(topicInfo)
	if coordErr != nil {
		coordLog.Infof("failed notify disable write while election: %v", coordErr)
		return coordErr
	}
	// choose another leader in ISR list, and add new node to ISR
	// list.
	newLeader, newestLogID, coordErr := self.chooseNewLeaderFromISR(topicInfo, currentNodes)
	if coordErr != nil {
		return coordErr
	}
	topicInfo.Leader = newLeader

	coordErr = self.makeNewTopicLeaderAcknowledged(topicInfo, newLeader, newestLogID)
	if coordErr != nil {
		return coordErr
	}
	// Is it possible the topic info changed but no leadership watch trigger?
	go self.triggerCheckTopics(time.Second * 5)

	return nil
}

func (self *NsqLookupCoordinator) handleRemoveFailedISRNodes(failedNodes []string, topicInfo *TopicPartionMetaInfo) {
	if len(failedNodes) == 0 {
		return
	}
	newISR := FilterList(topicInfo.ISR, failedNodes)
	if len(newISR) == 0 {
		coordLog.Infof("no node left in isr if removing failed")
		return
	}
	topicInfo.ISR = newISR
	if len(topicInfo.ISR) <= topicInfo.Replica/2 {
		coordLog.Infof("no enough isr node while removing the failed nodes. %v", topicInfo.ISR)
	}
	topicInfo.CatchupList = MergeList(topicInfo.CatchupList, failedNodes)
	coordLog.Infof("topic info updated: %v", topicInfo)
	err := self.leadership.UpdateTopicNodeInfo(topicInfo.Name, topicInfo.Partition, topicInfo, topicInfo.Epoch)
	if err != nil {
		coordLog.Infof("update topic node isr failed: %v", err.Error())
		return
	}
	self.notifyTopicMetaInfo(topicInfo)
}

func (self *NsqLookupCoordinator) handleTopicMigrate(topicInfo *TopicPartionMetaInfo, currentNodes map[string]NsqdNodeInfo) {
	if _, ok := currentNodes[topicInfo.Leader]; !ok {
		coordLog.Warningf("topic leader node is down: %v", topicInfo)
		return
	}
	isrChanged := false
	for _, replica := range topicInfo.ISR {
		if _, ok := currentNodes[replica]; !ok {
			coordLog.Warningf("topic %v isr node %v is lost.", topicInfo.GetTopicDesp(), replica)
			isrChanged = true
		}
	}
	if isrChanged {
		// will re-check to handle isr node failure
		return
	}

	catchupChanged := false
	aliveCatchup := 0
	for _, n := range topicInfo.CatchupList {
		if _, ok := currentNodes[n]; ok {
			aliveCatchup++
		} else {
			coordLog.Infof("topic %v catchup node %v is lost.", topicInfo.GetTopicDesp(), n)
		}
	}
	topicNsqdNum := len(topicInfo.ISR) + aliveCatchup
	if topicNsqdNum < topicInfo.Replica {
		for i := topicNsqdNum; i < topicInfo.Replica; i++ {
			// should exclude the current isr and catchup node
			n, err := self.allocNodeForTopic(topicInfo, currentNodes)
			if err != nil {
				coordLog.Infof("failed to get a new catchup for topic: %v", topicInfo.GetTopicDesp())
			} else {
				topicInfo.CatchupList = append(topicInfo.CatchupList, n.GetID())
				catchupChanged = true
			}
		}
	}
	if catchupChanged {
		err := self.leadership.UpdateTopicNodeInfo(topicInfo.Name, topicInfo.Partition, topicInfo, int(topicInfo.Epoch))
		if err != nil {
			coordLog.Infof("update topic node info failed: %v", err.Error())
			return
		}
	}
	coordLog.Infof("notify meta info for migrate: %v", topicInfo)
	self.notifyTopicMetaInfo(topicInfo)
}

// make sure the previous leader is not holding its leader session.
func (self *NsqLookupCoordinator) waitOldLeaderRelease(topicInfo *TopicPartionMetaInfo) error {
	err := RetryWithTimeout(func() error {
		_, err := self.leadership.GetTopicLeaderSession(topicInfo.Name, topicInfo.Partition)
		if err == ErrSessionNotExist {
			return nil
		}
		time.Sleep(time.Second)
		return err
	})
	return err
}

func (self *NsqLookupCoordinator) chooseNewLeaderFromISR(topicInfo *TopicPartionMetaInfo, currentNodes map[string]NsqdNodeInfo) (string, int64, *CoordErr) {
	// choose another leader in ISR list, and add new node to ISR
	// list.
	newestReplicas := make([]string, 0)
	newestLogID := int64(0)
	for _, replica := range topicInfo.ISR {
		if _, ok := currentNodes[replica]; !ok {
			continue
		}
		if replica == topicInfo.Leader {
			continue
		}
		cid, err := self.getNsqdLastCommitLogID(replica, topicInfo)
		if err != nil {
			coordLog.Infof("failed to get log id on replica: %v, %v", replica, err)
			continue
		}
		if cid > newestLogID {
			newestReplicas = newestReplicas[0:0]
			newestReplicas = append(newestReplicas, replica)
			newestLogID = cid
		} else if cid == newestLogID {
			newestReplicas = append(newestReplicas, replica)
		}
	}
	// select the least load factor node
	newLeader := ""
	if len(newestReplicas) == 1 {
		newLeader = newestReplicas[0]
	} else {
		minLF := 100.0
		for _, replica := range newestReplicas {
			stat, err := self.getNsqdTopicStat(currentNodes[replica])
			if err != nil {
				continue
			}
			_, lf := stat.GetNodeLeaderLoadFactor()
			if lf < minLF {
				newLeader = replica
			}
		}
	}
	if newLeader == "" {
		coordLog.Warningf("No leader can be elected. current topic info: %v", topicInfo)
		return "", 0, ErrNoLeaderCanBeElected
	}
	coordLog.Infof("new leader %v found with commit id: %v", newLeader, newestLogID)
	return newLeader, newestLogID, nil
}

func (self *NsqLookupCoordinator) makeNewTopicLeaderAcknowledged(topicInfo *TopicPartionMetaInfo, newLeader string, newestLogID int64) *CoordErr {
	topicInfo.Leader = newLeader
	err := self.leadership.UpdateTopicNodeInfo(topicInfo.Name, topicInfo.Partition, topicInfo, topicInfo.Epoch)
	if err != nil {
		coordLog.Infof("update topic node info failed: %v", err)
		return &CoordErr{err.Error(), RpcNoErr, CoordCommonErr}
	}
	self.notifyTopicMetaInfo(topicInfo)

	var leaderSession *TopicLeaderSession
	for {
		leaderSession, err = self.leadership.GetTopicLeaderSession(topicInfo.Name, topicInfo.Partition)
		if err != nil {
			coordLog.Infof("topic leader session still missing")
			self.nodesMutex.RLock()
			_, ok := self.nsqdNodes[topicInfo.Leader]
			self.nodesMutex.RUnlock()
			if !ok {
				coordLog.Warningf("leader is lost while waiting acknowledge")
				return ErrLeaderNodeLost
			}
			self.notifyISRTopicMetaInfo(topicInfo)
			time.Sleep(time.Second)
		} else {
			coordLog.Infof("topic leader session found: %v", leaderSession)
			break
		}
	}
	return nil
}

func (self *NsqLookupCoordinator) acquireRpcClient(nid string) (*NsqdRpcClient, *CoordErr) {
	self.nodesMutex.RLock()
	currentNodes := self.nsqdNodes
	self.nodesMutex.RUnlock()

	self.rpcMutex.Lock()
	defer self.rpcMutex.Unlock()
	c, _ := self.nsqdRpcClients[nid]
	if c == nil {
		n, ok := currentNodes[nid]
		if !ok {
			coordLog.Infof("rpc node not found: %v", nid)
			return nil, ErrNodeNotFound
		}
		var err error
		c, err = NewNsqdRpcClient(net.JoinHostPort(n.NodeIp, n.RpcPort), RPC_TIMEOUT)
		if err != nil {
			coordLog.Infof("rpc node %v client init failed : %v", nid, err)
			return nil, &CoordErr{err.Error(), RpcNoErr, CoordNetErr}
		}
		self.nsqdRpcClients[nid] = c
	}
	return c, nil
}

func (self *NsqLookupCoordinator) notifyEnableTopicWrite(topicInfo *TopicPartionMetaInfo) *CoordErr {
	for _, node := range topicInfo.ISR {
		if node == topicInfo.Leader {
			continue
		}
		c, err := self.acquireRpcClient(node)
		if err != nil {
			return err
		}
		err = c.EnableTopicWrite(self.leaderNode.Epoch, topicInfo)
		if err != nil {
			return err
		}
	}
	c, err := self.acquireRpcClient(topicInfo.Leader)
	if err != nil {
		return err
	}
	err = c.EnableTopicWrite(self.leaderNode.Epoch, topicInfo)
	return err
}

// each time change leader or isr list, make sure disable write.
// Because we need make sure the new leader and isr is in sync before accepting the
// write request.
func (self *NsqLookupCoordinator) notifyLeaderDisableTopicWrite(topicInfo *TopicPartionMetaInfo) *CoordErr {
	c, err := self.acquireRpcClient(topicInfo.Leader)
	if err != nil {
		coordLog.Infof("failed to get rpc client: %v, %v", err, topicInfo.Leader)
		return err
	}
	err = c.DisableTopicWrite(self.leaderNode.Epoch, topicInfo)
	return err
}

func (self *NsqLookupCoordinator) notifyISRDisableTopicWrite(topicInfo *TopicPartionMetaInfo) *CoordErr {
	for _, node := range topicInfo.ISR {
		if node == topicInfo.Leader {
			continue
		}
		c, err := self.acquireRpcClient(node)
		if err != nil {
			return err
		}
		err = c.DisableTopicWrite(self.leaderNode.Epoch, topicInfo)
		if err != nil {
			return err
		}
	}
	return nil
}

func (self *NsqLookupCoordinator) getNsqdLastCommitLogID(nid string, topicInfo *TopicPartionMetaInfo) (int64, *CoordErr) {
	c, err := self.acquireRpcClient(nid)
	if err != nil {
		return 0, err
	}
	return c.GetLastCommitLogID(topicInfo)
}

func (self *NsqLookupCoordinator) getExcludeNodesForTopic(topicInfo *TopicPartionMetaInfo) map[string]struct{} {
	excludeNodes := make(map[string]struct{})
	excludeNodes[topicInfo.Leader] = struct{}{}
	for _, v := range topicInfo.ISR {
		excludeNodes[v] = struct{}{}
	}
	for _, v := range topicInfo.CatchupList {
		excludeNodes[v] = struct{}{}
	}
	// exclude other partition node with the same topic
	num, err := self.leadership.GetTopicPartitionNum(topicInfo.Name)
	if err != nil {
		coordLog.Infof("failed get the partition num: %v", err)
		return excludeNodes
	}
	for i := 0; i < num; i++ {
		topicInfo, err := self.leadership.GetTopicInfo(topicInfo.Name, i)
		if err != nil {
			continue
		}
		excludeNodes[topicInfo.Leader] = struct{}{}
		for _, v := range topicInfo.ISR {
			excludeNodes[v] = struct{}{}
		}
		for _, v := range topicInfo.CatchupList {
			excludeNodes[v] = struct{}{}
		}
	}

	return excludeNodes
}

func (self *NsqLookupCoordinator) allocNodeForTopic(topicInfo *TopicPartionMetaInfo, currentNodes map[string]NsqdNodeInfo) (*NsqdNodeInfo, *CoordErr) {
	// collect the nsqd data, check if any node has the topic data already.
	var chosenNode NsqdNodeInfo
	var chosenStat *NodeTopicStats

	excludeNodes := self.getExcludeNodesForTopic(topicInfo)

	for nodeID, nodeInfo := range currentNodes {
		if _, ok := excludeNodes[nodeID]; ok {
			continue
		}
		topicStat, err := self.getNsqdTopicStat(nodeInfo)
		if err != nil {
			coordLog.Infof("failed to get topic status for this node: %v", nodeInfo)
			continue
		}
		if chosenNode.ID == "" {
			chosenNode = nodeInfo
			chosenStat = topicStat
			continue
		}
		if topicStat.SlaveLessLoader(chosenStat) {
			chosenNode = nodeInfo
			chosenStat = topicStat
		}
	}
	if chosenNode.ID == "" {
		coordLog.Infof("no more available node for topic: %v, excluding nodes: %v, all nodes: %v", topicInfo.GetTopicDesp(), excludeNodes, currentNodes)
		return nil, ErrNodeUnavailable
	}
	coordLog.Infof("node %v is alloc for topic: %v", chosenNode, topicInfo.GetTopicDesp())
	return &chosenNode, nil
}

func (self *NsqLookupCoordinator) getNsqdTopicStat(node NsqdNodeInfo) (*NodeTopicStats, error) {
	c, err := self.acquireRpcClient(node.GetID())
	if err != nil {
		return nil, err
	}
	return c.GetTopicStats("")
}

//check period for the data balance.
func (self *NsqLookupCoordinator) balanceTopicData(monitorChan chan struct{}) {
	ticker := time.NewTicker(time.Minute * 10)
	defer func() {
		ticker.Stop()
		coordLog.Infof("balance check exit.")
	}()
	for {
		select {
		case <-monitorChan:
			return
		case <-ticker.C:
			// only balance at 2:00~6:00
			if time.Now().Hour() > 5 || time.Now().Hour() < 2 {
				continue
			}
			avgLoad := 0.0
			minLoad := 0.0
			_ = minLoad
			maxLoad := 0.0
			_ = maxLoad
			// TODO: if max load is 4 times more than avg load, we need move some
			// leader from max to min load node one by one.
			// if min load is 4 times less than avg load, we can move some
			// leader to this min load node.
			_ = avgLoad
			// check each node
			coordLog.Infof("begin checking balance of topic data...")
			// TODO: balance should restrict to some data and stop if time is over 6:00
			self.nodesMutex.RLock()
			currentNodes := self.nsqdNodes
			self.nodesMutex.RUnlock()
			for nodeID, nodeInfo := range currentNodes {
				topicStat, err := self.getNsqdTopicStat(nodeInfo)
				if err != nil {
					coordLog.Infof("failed to get node topic status while checking balance: %v", nodeID)
					continue
				}
				_, leaderLF := topicStat.GetNodeLeaderLoadFactor()
				coordLog.Infof("nsqd node %v load factor is : %v, %v", nodeID, leaderLF, topicStat.GetNodeLoadFactor())
			}
		}
	}
}

// init leader node and isr list for the empty topic
func (self *NsqLookupCoordinator) allocTopicLeaderAndISR(currentNodes map[string]NsqdNodeInfo, replica int, partitionNum int, existPart map[int]*TopicPartionMetaInfo) ([]string, [][]string, error) {
	if len(currentNodes) < replica || len(currentNodes) < partitionNum {
		coordLog.Infof("nodes %v is less than replica %v or partition %v", len(currentNodes), replica, partitionNum)
		return nil, nil, ErrNodeUnavailable
	}
	if len(currentNodes) < replica*partitionNum {
		coordLog.Infof("nodes is less than replica*partition")
		return nil, nil, ErrNodeUnavailable
	}
	existLeaders := make(map[string]struct{})
	existSlaves := make(map[string]struct{})
	for _, topicInfo := range existPart {
		for _, n := range topicInfo.ISR {
			if n == topicInfo.Leader {
				existLeaders[n] = struct{}{}
			} else {
				existSlaves[n] = struct{}{}
			}
		}
	}
	nodeTopicStats := make([]NodeTopicStats, 0, len(currentNodes))
	for _, nodeInfo := range currentNodes {
		stats, err := self.getNsqdTopicStat(nodeInfo)
		if err != nil {
			coordLog.Infof("got topic status for node %v failed: %v", nodeInfo.GetID(), err)
			continue
		}
		nodeTopicStats = append(nodeTopicStats, *stats)
	}
	if len(nodeTopicStats) < partitionNum*replica {
		return nil, nil, ErrNodeUnavailable
	}
	leaderSort := func(l, r *NodeTopicStats) bool {
		return l.LeaderLessLoader(r)
	}
	By(leaderSort).Sort(nodeTopicStats)
	leaders := make([]string, partitionNum)
	p := 0
	currentSelect := 0
	for p < partitionNum {
		if elem, ok := existPart[p]; ok {
			leaders[p] = elem.Leader
		} else {
			for {
				if currentSelect >= len(nodeTopicStats) {
					coordLog.Infof("not enough nodes for leaders")
					return nil, nil, ErrNodeUnavailable
				}
				nodeInfo := nodeTopicStats[currentSelect]
				currentSelect++
				if _, ok := existLeaders[nodeInfo.NodeID]; ok {
					continue
				}
				// TODO: should slave can be used for other leader?
				if _, ok := existSlaves[nodeInfo.NodeID]; ok {
					continue
				}
				leaders[p] = nodeInfo.NodeID
				existLeaders[nodeInfo.NodeID] = struct{}{}
				break
			}
		}
		p++
	}
	p = 0
	currentSelect = 0
	slaveSort := func(l, r *NodeTopicStats) bool {
		return l.SlaveLessLoader(r)
	}
	By(slaveSort).Sort(nodeTopicStats)

	isrlist := make([][]string, partitionNum)
	for p < partitionNum {
		isr := make([]string, 0, replica)
		isr = append(isr, leaders[p])
		if elem, ok := existPart[p]; ok {
			isr = elem.ISR
		} else {
			for {
				if currentSelect >= len(nodeTopicStats) {
					coordLog.Infof("not enough nodes for slaves")
					return nil, nil, ErrNodeUnavailable
				}
				nodeInfo := nodeTopicStats[currentSelect]
				currentSelect++
				if nodeInfo.NodeID == leaders[p] {
					continue
				}
				if _, ok := existSlaves[nodeInfo.NodeID]; ok {
					continue
				}
				// TODO: should slave can be used for other leader?
				if _, ok := existLeaders[nodeInfo.NodeID]; ok {
					continue
				}
				existSlaves[nodeInfo.NodeID] = struct{}{}
				isr = append(isr, nodeInfo.NodeID)
				if len(isr) >= replica {
					break
				}
			}
		}
		isrlist[p] = isr
		p++
	}
	coordLog.Infof("topic selected leader: %v, topic selected isr : %v", leaders, isrlist)
	return leaders, isrlist, nil
}

func (self *NsqLookupCoordinator) CreateTopic(topic string, partitionNum int, replica int) error {
	if self.leaderNode.GetID() != self.myNode.GetID() {
		coordLog.Infof("not leader while create topic")
		return ErrNotNsqLookupLeader
	}

	coordLog.Infof("create topic: %v-%v, replica: %v", topic, partitionNum, replica)
	if ok, _ := self.leadership.IsExistTopic(topic); !ok {
		err := self.leadership.CreateTopic(topic, partitionNum, replica)
		if err != nil {
			coordLog.Infof("create topic key %v failed :%v", topic, err)
			return err
		}
	}

	existPart := make(map[int]*TopicPartionMetaInfo)
	for i := 0; i < partitionNum; i++ {
		err := self.leadership.CreateTopicPartition(topic, i)
		if err == ErrAlreadyExist {
			coordLog.Infof("create topic partition already exist %v-%v: %v", topic, i, err.Error())
			t, err := self.leadership.GetTopicInfo(topic, i)
			if err != nil {
				coordLog.Warningf("exist topic partition failed to get info: %v", err)
				return err
			}
			existPart[i] = t
			continue
		}
		if err != nil {
			coordLog.Warningf("failed to create topic %v-%v: %v", topic, i, err.Error())
			return err
		} else {
			if self.nsqdMonitorChan != nil {
				go self.watchTopicLeaderSession(self.nsqdMonitorChan, topic, i)
			}
		}
	}
	self.nodesMutex.RLock()
	currentNodes := self.nsqdNodes
	self.nodesMutex.RUnlock()
	leaders, isrList, err := self.allocTopicLeaderAndISR(currentNodes, replica, partitionNum, existPart)
	if err != nil {
		coordLog.Infof("failed to alloc nodes for topic: %v", err)
		return err
	}
	if len(leaders) != partitionNum || len(isrList) != partitionNum {
		return ErrNodeUnavailable
	}
	if err != nil {
		coordLog.Infof("failed alloc nodes for topic: %v", err)
		return err
	}
	for i := 0; i < partitionNum; i++ {
		if _, ok := existPart[i]; ok {
			continue
		}
		oldInfo, err := self.leadership.GetTopicInfo(topic, i)
		if err != nil {
			coordLog.Infof("failed get topic info: %v", err)
			continue
		}
		var tmpTopicInfo TopicPartionMetaInfo
		tmpTopicInfo = *oldInfo
		tmpTopicInfo.Name = topic
		tmpTopicInfo.Partition = i
		tmpTopicInfo.Replica = replica
		tmpTopicInfo.ISR = isrList[i]
		tmpTopicInfo.Leader = leaders[i]

		err = self.leadership.UpdateTopicNodeInfo(topic, i, &tmpTopicInfo, tmpTopicInfo.Epoch)
		if err != nil {
			coordLog.Infof("failed update info for topic : %v-%v, %v", topic, i, err)
			continue
		}
		rpcErr := self.notifyTopicMetaInfo(&tmpTopicInfo)
		if rpcErr != nil {
			coordLog.Warningf("failed notify topic info : %v", rpcErr)
		} else {
			coordLog.Infof("topic %v init successful.", tmpTopicInfo)
		}
	}
	return nil
}

// if any failed to enable topic write , we need start a new join isr session to
// make sure all the isr nodes are ready for write
// should disable write before call
func (self *NsqLookupCoordinator) revokeEnableTopicWrite(topic string, partition int, isLeadershipWait bool) error {
	coordLog.Infof("revoke the topic to enable write: %v-%v", topic, partition)
	topicInfo, err := self.leadership.GetTopicInfo(topic, partition)
	if err != nil {
		coordLog.Infof("get topic info failed : %v", err.Error())
		return err
	}
	leaderSession, err := self.leadership.GetTopicLeaderSession(topic, partition)
	if err != nil {
		coordLog.Infof("failed to get leader session: %v", err)
		return err
	}

	self.joinStateMutex.Lock()
	state, ok := self.joinISRState[topicInfo.GetTopicDesp()]
	if !ok {
		state = &JoinISRState{}
		self.joinISRState[topicInfo.GetTopicDesp()] = state
	}
	self.joinStateMutex.Unlock()
	state.Lock()
	defer state.Unlock()
	if state.waitingJoin {
		coordLog.Warningf("request join isr while is waiting joining: %v", state)
		if isLeadershipWait {
			coordLog.Warningf("interrupt the current join wait since the leader is waiting confirmation")
		} else {
			return ErrWaitingJoinISR
		}
	}
	if state.doneChan != nil {
		close(state.doneChan)
		state.doneChan = nil
	}
	state.waitingJoin = false
	state.waitingSession = ""

	rpcErr := self.notifyLeaderDisableTopicWrite(topicInfo)
	if rpcErr != nil {
		coordLog.Infof("try disable write for topic %v failed: %v", topicInfo, rpcErr)
		go self.triggerCheckTopics(time.Second)
		return rpcErr
	}

	if rpcErr = self.notifyISRDisableTopicWrite(topicInfo); rpcErr != nil {
		coordLog.Infof("try disable isr write for topic %v failed: %v", topicInfo, rpcErr)
		go self.triggerCheckTopics(time.Second * 3)
		return rpcErr
	}
	state.isLeadershipWait = isLeadershipWait
	self.initJoinStateAndWait(topicInfo, leaderSession, state)

	return nil
}

func (self *NsqLookupCoordinator) initJoinStateAndWait(topicInfo *TopicPartionMetaInfo, leaderSession *TopicLeaderSession, state *JoinISRState) {
	state.waitingJoin = true
	state.waitingStart = time.Now()
	state.waitingSession = topicInfo.Leader + ","
	for _, s := range topicInfo.ISR {
		state.waitingSession += s + ","
	}
	state.waitingSession += strconv.Itoa(int(topicInfo.Epoch)) + "-" + strconv.Itoa(int(leaderSession.LeaderEpoch)) + ","
	state.waitingSession += state.waitingStart.String()

	state.doneChan = make(chan struct{})
	state.readyNodes = make(map[string]struct{})
	state.readyNodes[topicInfo.Leader] = struct{}{}

	coordLog.Infof("topic %v isr waiting session init : %v", topicInfo.GetTopicDesp(), state)
	if len(topicInfo.ISR) <= 1 {
		rpcErr := self.notifyEnableTopicWrite(topicInfo)
		if rpcErr != nil {
			coordLog.Warningf("failed to enable write for topic: %v, %v ", topicInfo.GetTopicDesp(), rpcErr)
		}
		state.waitingJoin = false
		state.waitingSession = ""
		if state.doneChan != nil {
			close(state.doneChan)
			state.doneChan = nil
		}
		coordLog.Infof("isr join state is ready since only leader in isr")
		return
	} else {
		go self.waitForFinalSyncedISR(*topicInfo, *leaderSession, state, state.doneChan)
	}
	self.notifyTopicMetaInfo(topicInfo)
	self.notifyTopicLeaderSession(topicInfo, leaderSession, state.waitingSession)
}

// some failed rpc means lost, we should always try to notify to the node when they are available
func (self *NsqLookupCoordinator) rpcFailRetryFunc(monitorChan chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-monitorChan:
			return
		case <-ticker.C:
			failList := make(map[string]RpcFailedInfo, 0)
			self.failedRpcMutex.Lock()
			for _, v := range self.failedRpcList {
				failList[v.nodeID+v.topic+strconv.Itoa(v.partition)] = v
			}
			if len(failList) > 0 {
				coordLog.Infof("failed rpc total: %v, %v", len(self.failedRpcList), len(failList))
			}
			self.failedRpcList = self.failedRpcList[0:0]
			self.failedRpcMutex.Unlock()
			epoch := self.leaderNode.Epoch
			for _, info := range failList {
				coordLog.Infof("retry failed rpc call for topic: %v", info)
				topicInfo, err := self.leadership.GetTopicInfo(info.topic, info.partition)
				if err != nil {
					// TODO: ignore if not exist on etcd
					self.addRetryFailedRpc(info.topic, info.partition, info.nodeID)
					continue
				}
				c, rpcErr := self.acquireRpcClient(info.nodeID)
				if rpcErr != nil {
					self.addRetryFailedRpc(info.topic, info.partition, info.nodeID)
					continue
				}
				rpcErr = c.UpdateTopicInfo(epoch, topicInfo)
				if rpcErr != nil {
					self.addRetryFailedRpc(info.topic, info.partition, info.nodeID)
					continue
				}
				leaderSession, err := self.leadership.GetTopicLeaderSession(info.topic, info.partition)
				if err != nil {
					self.addRetryFailedRpc(info.topic, info.partition, info.nodeID)
					continue
				}
				rpcErr = c.NotifyTopicLeaderSession(epoch, topicInfo, leaderSession, "")
				if rpcErr != nil {
					self.addRetryFailedRpc(info.topic, info.partition, info.nodeID)
					continue
				}
			}
		}
	}
}

func (self *NsqLookupCoordinator) doNotifyToNsqdNodes(nodes []string, notifyRpcFunc func(string) *CoordErr) *CoordErr {
	self.nodesMutex.RLock()
	currentNodes := self.nsqdNodes
	self.nodesMutex.RUnlock()
	for _, n := range nodes {
		node, ok := currentNodes[n]
		if !ok {
			return ErrNodeNotFound
		}
		err := notifyRpcFunc(node.GetID())
		if err != nil {
			coordLog.Infof("notify to nsqd node %v failed: %v", node, err)
			return err
		}
	}
	return nil
}

func (self *NsqLookupCoordinator) doNotifyToSingleNsqdNode(nodeID string, notifyRpcFunc func(string) *CoordErr) *CoordErr {
	self.nodesMutex.RLock()
	node, ok := self.nsqdNodes[nodeID]
	self.nodesMutex.RUnlock()
	if !ok {
		return ErrNodeNotFound
	}
	err := notifyRpcFunc(node.GetID())
	if err != nil {
		coordLog.Infof("notify to nsqd node %v failed: %v", node, err)
	}
	return err
}

func (self *NsqLookupCoordinator) doNotifyToTopicLeaderThenOthers(leader string, others []string, notifyRpcFunc func(string) *CoordErr) *CoordErr {
	err := self.doNotifyToSingleNsqdNode(leader, notifyRpcFunc)
	if err != nil {
		coordLog.Infof("notify to topic leader %v failed: %v", leader, err)
		return err
	}
	return self.doNotifyToNsqdNodes(others, notifyRpcFunc)
}

func (self *NsqLookupCoordinator) notifyTopicLeaderSession(topicInfo *TopicPartionMetaInfo, leaderSession *TopicLeaderSession, joinSession string) *CoordErr {
	others := getOthersExceptLeader(topicInfo)
	coordLog.Infof("notify topic leader session changed: %v, %v, others: %v", topicInfo.GetTopicDesp(), leaderSession.Session, others)
	err := self.doNotifyToTopicLeaderThenOthers(topicInfo.Leader, others, func(nid string) *CoordErr {
		return self.sendTopicLeaderSessionToNsqd(self.leaderNode.Epoch, nid, topicInfo, leaderSession, joinSession)
	})
	return err
}

func (self *NsqLookupCoordinator) notifyISRTopicMetaInfo(topicInfo *TopicPartionMetaInfo) *CoordErr {
	rpcErr := self.doNotifyToNsqdNodes(topicInfo.ISR, func(nid string) *CoordErr {
		return self.sendTopicInfoToNsqd(self.leaderNode.Epoch, nid, topicInfo)
	})
	if rpcErr != nil {
		coordLog.Infof("notify isr for topic meta info failed: %v", rpcErr)
	}
	return rpcErr
}

func (self *NsqLookupCoordinator) notifyTopicMetaInfo(topicInfo *TopicPartionMetaInfo) *CoordErr {
	others := getOthersExceptLeader(topicInfo)
	coordLog.Infof("notify topic meta info changed: %v", topicInfo)
	rpcErr := self.doNotifyToTopicLeaderThenOthers(topicInfo.Leader, others, func(nid string) *CoordErr {
		return self.sendTopicInfoToNsqd(self.leaderNode.Epoch, nid, topicInfo)
	})
	if rpcErr != nil {
		coordLog.Infof("notify topic meta info failed: %v", rpcErr)
	}
	return rpcErr
}

func (self *NsqLookupCoordinator) notifyOldNsqdsForTopicMetaInfo(topicInfo *TopicPartionMetaInfo, oldNodes []string) *CoordErr {
	return self.doNotifyToNsqdNodes(oldNodes, func(nid string) *CoordErr {
		return self.sendTopicInfoToNsqd(self.leaderNode.Epoch, nid, topicInfo)
	})
}

func (self *NsqLookupCoordinator) notifySingleNsqdForTopicReload(topicInfo *TopicPartionMetaInfo, nodeID string) *CoordErr {
	rpcErr := self.sendTopicInfoToNsqd(self.leaderNode.Epoch, nodeID, topicInfo)
	if rpcErr != nil {
		return rpcErr
	}
	leaderSession, err := self.leadership.GetTopicLeaderSession(topicInfo.Name, topicInfo.Partition)
	if err != nil {
		return &CoordErr{err.Error(), RpcCommonErr, CoordNetErr}
	}
	return self.sendTopicLeaderSessionToNsqd(self.leaderNode.Epoch, nodeID, topicInfo, leaderSession, "")
}

func (self *NsqLookupCoordinator) notifyAllNsqdsForTopicReload(topicInfo *TopicPartionMetaInfo) *CoordErr {
	rpcErr := self.notifyTopicMetaInfo(topicInfo)
	if rpcErr != nil {
		return rpcErr
	}
	leaderSession, err := self.leadership.GetTopicLeaderSession(topicInfo.Name, topicInfo.Partition)
	if err == nil {
		self.notifyTopicLeaderSession(topicInfo, leaderSession, "")
	} else {
		coordLog.Infof("get leader session failed: %v", err)
	}
	return nil
}

func (self *NsqLookupCoordinator) addRetryFailedRpc(topic string, partition int, nid string) {
	failed := RpcFailedInfo{
		nodeID:    nid,
		topic:     topic,
		partition: partition,
		failTime:  time.Now(),
	}
	self.failedRpcMutex.Lock()
	self.failedRpcList = append(self.failedRpcList, failed)
	coordLog.Infof("failed rpc added: %v, total: %v", failed, len(self.failedRpcList))
	self.failedRpcMutex.Unlock()
}

func (self *NsqLookupCoordinator) sendTopicLeaderSessionToNsqd(epoch int, nid string, topicInfo *TopicPartionMetaInfo,
	leaderSession *TopicLeaderSession, joinSession string) *CoordErr {
	c, err := self.acquireRpcClient(nid)
	if err != nil {
		self.addRetryFailedRpc(topicInfo.Name, topicInfo.Partition, nid)
		return err
	}
	err = c.NotifyTopicLeaderSession(epoch, topicInfo, leaderSession, joinSession)
	if err != nil {
		self.addRetryFailedRpc(topicInfo.Name, topicInfo.Partition, nid)
	}
	return err
}

func (self *NsqLookupCoordinator) sendTopicInfoToNsqd(epoch int, nid string, topicInfo *TopicPartionMetaInfo) *CoordErr {
	c, rpcErr := self.acquireRpcClient(nid)
	if rpcErr != nil {
		self.addRetryFailedRpc(topicInfo.Name, topicInfo.Partition, nid)
		return rpcErr
	}
	rpcErr = c.UpdateTopicInfo(epoch, topicInfo)
	if rpcErr != nil {
		coordLog.Infof("failed to update topic info: %v", rpcErr)
		self.addRetryFailedRpc(topicInfo.Name, topicInfo.Partition, nid)
	}
	return rpcErr
}

func (self *NsqLookupCoordinator) handleRequestJoinCatchup(topic string, partition int, nid string) *CoordErr {
	var topicInfo *TopicPartionMetaInfo
	var err error
	topicInfo, err = self.leadership.GetTopicInfo(topic, partition)
	if err != nil {
		coordLog.Infof("failed to get topic info: %v", err)
		return &CoordErr{err.Error(), RpcCommonErr, CoordCommonErr}
	}
	if FindSlice(topicInfo.ISR, nid) != -1 {
		return &CoordErr{"catchup node should not in the isr", RpcCommonErr, CoordCommonErr}
	}
	if FindSlice(topicInfo.CatchupList, nid) == -1 {
		topicInfo.CatchupList = append(topicInfo.CatchupList, nid)
		err = self.leadership.UpdateTopicNodeInfo(topic, partition, topicInfo, topicInfo.Epoch)
		if err != nil {
			coordLog.Infof("failed to update catchup list: %v", err)
			return &CoordErr{err.Error(), RpcCommonErr, CoordNetErr}
		}
	}
	go self.notifyTopicMetaInfo(topicInfo)
	return nil
}

func (self *NsqLookupCoordinator) prepareJoinState(topic string, partition int) (*TopicPartionMetaInfo, *TopicLeaderSession, *JoinISRState, *CoordErr) {
	topicInfo, err := self.leadership.GetTopicInfo(topic, partition)
	if err != nil {
		coordLog.Infof("failed to get topic info: %v", err)
		return nil, nil, nil, &CoordErr{err.Error(), RpcCommonErr, CoordCommonErr}
	}
	leaderSession, err := self.leadership.GetTopicLeaderSession(topic, partition)
	if err != nil {
		coordLog.Infof("failed to get leader session: %v", err)
		return nil, nil, nil, &CoordErr{err.Error(), RpcCommonErr, CoordElectionTmpErr}
	}

	self.joinStateMutex.Lock()
	state, ok := self.joinISRState[topicInfo.GetTopicDesp()]
	if !ok {
		state = &JoinISRState{}
		self.joinISRState[topicInfo.GetTopicDesp()] = state
	}
	self.joinStateMutex.Unlock()

	return topicInfo, leaderSession, state, nil
}

func (self *NsqLookupCoordinator) handleRequestJoinISR(topic string, partition int, nodeID string) *CoordErr {
	// 1. got join isr request, check valid, should be in catchup list.
	// 2. notify the topic leader disable write
	// 3. add the node to ISR and remove from CatchupList.
	// 4. insert wait join session, notify all nodes for the new isr
	// 5. wait on the join session until all the new isr is ready (got the ready notify from isr)
	// 6. timeout or done, clear current join session, (only keep isr that got ready notify, shoud be quorum), enable write
	topicInfo, leaderSession, state, coordErr := self.prepareJoinState(topic, partition)
	if coordErr != nil {
		coordLog.Infof("failed to prepare join state: %v", coordErr)
		return coordErr
	}
	if FindSlice(topicInfo.CatchupList, nodeID) == -1 {
		coordLog.Infof("join isr node is not in catchup list.")
		return ErrJoinISRInvalid
	}
	coordLog.Infof("node %v request join isr for topic %v", nodeID, topicInfo.GetTopicDesp())

	// we go here to allow the rpc call from client can return ok immediately
	go func() {
		state.Lock()
		defer state.Unlock()
		if state.waitingJoin {
			coordLog.Warningf("failed request join isr because another is joining.")
			return
		}
		if state.doneChan != nil {
			close(state.doneChan)
			state.doneChan = nil
		}
		state.waitingJoin = false
		state.waitingSession = ""

		rpcErr := self.notifyLeaderDisableTopicWrite(topicInfo)
		if rpcErr != nil {
			coordLog.Warningf("try disable write for topic %v failed: %v", topicInfo.GetTopicDesp(), rpcErr)
			go self.triggerCheckTopics(time.Second)
			return
		}
		state.isLeadershipWait = false

		newCatchupList := make([]string, 0)
		for _, nid := range topicInfo.CatchupList {
			if nid == nodeID {
				continue
			}
			newCatchupList = append(newCatchupList, nid)
		}
		topicInfo.CatchupList = newCatchupList
		topicInfo.ISR = append(topicInfo.ISR, nodeID)
		err := self.leadership.UpdateTopicNodeInfo(topicInfo.Name, topicInfo.Partition, topicInfo, topicInfo.Epoch)
		if err != nil {
			coordLog.Infof("move catchup node to isr failed: %v", err)
			// continue here to allow the wait goroutine to handle the timeout
		}
		self.notifyTopicMetaInfo(topicInfo)

		if rpcErr = self.notifyISRDisableTopicWrite(topicInfo); rpcErr != nil {
			coordLog.Infof("try disable isr write for topic %v failed: %v", topicInfo, rpcErr)
			go self.triggerCheckTopics(time.Second * 3)
		}

		self.initJoinStateAndWait(topicInfo, leaderSession, state)
	}()
	return nil
}

func (self *NsqLookupCoordinator) handleReadyForISR(topic string, partition int, nodeID string,
	leaderSession TopicLeaderSession, joinISRSession string) *CoordErr {
	topicInfo, err := self.leadership.GetTopicInfo(topic, partition)
	if err != nil {
		coordLog.Infof("get topic info failed : %v", err.Error())
		return &CoordErr{err.Error(), RpcCommonErr, CoordCommonErr}
	}
	if FindSlice(topicInfo.ISR, nodeID) == -1 {
		coordLog.Infof("got ready for isr but not a isr node: %v, isr is: %v", nodeID, topicInfo.ISR)
		return ErrJoinISRInvalid
	}

	// check for state and should lock for the state to prevent others join isr.
	self.joinStateMutex.Lock()
	state, ok := self.joinISRState[topicInfo.GetTopicDesp()]
	self.joinStateMutex.Unlock()
	if !ok {
		coordLog.Warningf("failed join isr because the join state is not set: %v", topicInfo.GetTopicDesp())
		return ErrJoinISRInvalid
	}
	// we go here to allow the rpc call from client can return ok immediately
	go func() {
		state.Lock()
		defer state.Unlock()
		if !state.waitingJoin || state.waitingSession != joinISRSession {
			coordLog.Infof("state mismatch: %v", state, joinISRSession)
			return
		}

		coordLog.Infof("topic %v isr node %v ready for state: %v", topicInfo.GetTopicDesp(), nodeID, joinISRSession)
		state.readyNodes[nodeID] = struct{}{}
		for _, n := range topicInfo.ISR {
			if _, ok := state.readyNodes[n]; !ok {
				coordLog.Infof("node %v still waiting ready", n)
				return
			}
		}
		coordLog.Infof("topic %v isr new state is ready for all: %v", topicInfo.GetTopicDesp(), state)
		rpcErr := self.notifyEnableTopicWrite(topicInfo)
		if rpcErr != nil {
			coordLog.Warningf("failed to enable write for topic: %v, %v ", topicInfo.GetTopicDesp(), rpcErr)
			go self.triggerCheckTopics(time.Second * 3)
		}
		state.waitingJoin = false
		state.waitingSession = ""
		if state.doneChan != nil {
			close(state.doneChan)
			state.doneChan = nil
		}
		if len(topicInfo.ISR) >= topicInfo.Replica && len(topicInfo.CatchupList) > 0 {
			oldCatchupList := topicInfo.CatchupList
			coordLog.Infof("removing catchup since the isr is enough: %v", oldCatchupList)
			topicInfo.CatchupList = make([]string, 0)
			self.notifyTopicMetaInfo(topicInfo)
			self.notifyOldNsqdsForTopicMetaInfo(topicInfo, oldCatchupList)
		}
	}()
	return nil
}

func (self *NsqLookupCoordinator) resetJoinISRState(topicInfo TopicPartionMetaInfo, state *JoinISRState, updateISR bool) error {
	state.Lock()
	defer state.Unlock()
	if !state.waitingJoin {
		return nil
	}
	state.waitingJoin = false
	state.waitingSession = ""
	if state.doneChan != nil {
		close(state.doneChan)
		state.doneChan = nil
	}
	coordLog.Infof("topic: %v reset waiting join state: %v", topicInfo.GetTopicDesp(), state)
	ready := 0
	for _, n := range topicInfo.ISR {
		if _, ok := state.readyNodes[n]; ok {
			ready++
		}
	}

	if ready <= topicInfo.Replica/2 {
		coordLog.Infof("no enough ready isr while reset wait join: %v, expect: %v, actual: %v", state.waitingSession, topicInfo.ISR, state.readyNodes)
		// even timeout we can not enable this topic since no enough replicas
		// however, we should clear the join state so that we can try join new isr later
		go self.triggerCheckTopics(time.Second)
	} else {
		// some of isr failed to ready for the new isr state, we need rollback the new isr with the
		// isr got ready.
		coordLog.Infof("the join state: expect ready isr : %v, actual ready: %v ", topicInfo.ISR, state.readyNodes)
		if updateISR && ready != len(topicInfo.ISR) {
			oldISR := topicInfo.ISR
			newCatchupList := make(map[string]struct{})
			for _, n := range topicInfo.CatchupList {
				newCatchupList[n] = struct{}{}
			}
			topicInfo.ISR = make([]string, 0, len(state.readyNodes))
			for _, n := range oldISR {
				if _, ok := state.readyNodes[n]; ok {
					topicInfo.ISR = append(topicInfo.ISR, n)
				} else {
					newCatchupList[n] = struct{}{}
				}
			}
			topicInfo.CatchupList = make([]string, 0)
			for n, _ := range newCatchupList {
				topicInfo.CatchupList = append(topicInfo.CatchupList, n)
			}
			err := self.leadership.UpdateTopicNodeInfo(topicInfo.Name, topicInfo.Partition, &topicInfo, topicInfo.Epoch)
			if err != nil {
				coordLog.Infof("update topic info failed: %v", err)
				return err
			}
			rpcErr := self.notifyTopicMetaInfo(&topicInfo)
			if rpcErr != nil {
				coordLog.Infof("failed to notify new topic info: %v", topicInfo)
				go self.triggerCheckTopics(time.Second)
				return rpcErr
			}
		}
		rpcErr := self.notifyEnableTopicWrite(&topicInfo)
		if rpcErr != nil {
			coordLog.Warningf("failed to enable write :%v, %v", topicInfo.GetTopicDesp(), rpcErr)
			go self.triggerCheckTopics(time.Second * 3)
		}
	}

	return nil
}

func (self *NsqLookupCoordinator) waitForFinalSyncedISR(topicInfo TopicPartionMetaInfo, leaderSession TopicLeaderSession, state *JoinISRState, doneChan chan struct{}) {
	ticker := time.NewTicker(time.Second * 5)
	defer ticker.Stop()
	select {
	case <-ticker.C:
		coordLog.Infof("wait timeout for sync isr.")
	case <-doneChan:
		return
	}

	self.resetJoinISRState(topicInfo, state, true)
}

func (self *NsqLookupCoordinator) transferTopicLeader(topicInfo *TopicPartionMetaInfo) *CoordErr {
	if len(topicInfo.ISR) < 2 {
		return ErrNodeUnavailable
	}
	self.nodesMutex.RLock()
	currentNodes := self.nsqdNodes
	self.nodesMutex.RUnlock()
	// try
	newLeader, newestLogID, err := self.chooseNewLeaderFromISR(topicInfo, currentNodes)
	if err != nil {
		return err
	}
	if newLeader == "" {
		return ErrLeaderElectionFail
	}
	_, _, state, err := self.prepareJoinState(topicInfo.Name, topicInfo.Partition)
	if err != nil {
		coordLog.Infof("prepare join state failed: %v", err)
		return err
	}
	state.Lock()
	defer state.Unlock()
	if state.waitingJoin {
		coordLog.Warningf("failed because another is waiting join.")
		return ErrLeavingISRWait
	}
	if state.doneChan != nil {
		close(state.doneChan)
		state.doneChan = nil
	}
	state.waitingJoin = false
	state.waitingSession = ""

	rpcErr := self.notifyLeaderDisableTopicWrite(topicInfo)
	if rpcErr != nil {
		coordLog.Infof("disable write failed while transfer leader: %v", rpcErr)
		return rpcErr
	}
	newLeader, newestLogID, err = self.chooseNewLeaderFromISR(topicInfo, currentNodes)
	if err != nil {
		return err
	}

	if rpcErr = self.notifyISRDisableTopicWrite(topicInfo); rpcErr != nil {
		coordLog.Infof("try disable isr write for topic %v failed: %v", topicInfo, rpcErr)
		go self.triggerCheckTopics(time.Second * 3)
	}

	return self.makeNewTopicLeaderAcknowledged(topicInfo, newLeader, newestLogID)
}

func (self *NsqLookupCoordinator) handleLeaveFromISR(topic string, partition int, leader *TopicLeaderSession, nodeID string) *CoordErr {
	topicInfo, err := self.leadership.GetTopicInfo(topic, partition)
	if err != nil {
		coordLog.Infof("get topic info failed :%v", err)
		return &CoordErr{err.Error(), RpcCommonErr, CoordCommonErr}
	}
	if topicInfo.Leader == nodeID {
		coordLog.Infof("the leader node %v will leave the isr, prepare transfer leader", nodeID)
		coordErr := self.transferTopicLeader(topicInfo)
		if coordErr != nil {
			coordLog.Warningf("the leader can not be transferred currently: %v", coordErr)
			return coordErr
		}
	}
	if FindSlice(topicInfo.ISR, nodeID) == -1 {
		return nil
	}
	if len(topicInfo.ISR) <= topicInfo.Replica/2 {
		coordLog.Infof("no enough isr node, leaving should wait.")
		go self.triggerCheckTopics(time.Second)
		return ErrLeavingISRWait
	}

	if leader != nil {
		newestLeaderSession, err := self.leadership.GetTopicLeaderSession(topic, partition)
		if err != nil {
			coordLog.Infof("get leader session failed: %v.", err.Error())
			return &CoordErr{err.Error(), RpcCommonErr, CoordCommonErr}
		}
		if !leader.IsSame(newestLeaderSession) {
			return ErrNotTopicLeader
		}
	}
	newISR := make([]string, 0, len(topicInfo.ISR)-1)
	for _, n := range topicInfo.ISR {
		if n == nodeID {
			continue
		}
		newISR = append(newISR, n)
	}
	topicInfo.ISR = newISR
	topicInfo.CatchupList = append(topicInfo.CatchupList, nodeID)
	err = self.leadership.UpdateTopicNodeInfo(topicInfo.Name, topicInfo.Partition, topicInfo, topicInfo.Epoch)
	if err != nil {
		coordLog.Infof("remove node from isr failed: %v", err.Error())
		return &CoordErr{err.Error(), RpcCommonErr, CoordCommonErr}
	}

	go self.notifyTopicMetaInfo(topicInfo)
	coordLog.Infof("node %v removed by plan from topic isr: %v", nodeID, topicInfo)
	return nil
}

func (self *NsqLookupCoordinator) IsTopicLeader(topic string, part int, nid string) bool {
	t, err := self.leadership.GetTopicInfo(topic, part)
	if err != nil {
		return false
	}
	return t.Leader == nid
}