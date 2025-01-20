package raft

import (
	"errors"
	"log/slog"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/troygilman0/actormq"
)

const (
	minServersForElection    = 3
	minElectionTimeoutMs     = 150
	maxElectionTimeoutMs     = 300
	heartbeatTimeoutMs       = 50
	heartbeatTimeoutDuration = heartbeatTimeoutMs * time.Millisecond
	checkTimersInterval      = 10 * time.Millisecond
)

type (
	checkTimers = struct{}
)

type NodeConfig struct {
	DiscoveryPID *actor.PID
	Handler      CommandHandler
	Logger       *slog.Logger
}

type nodeActor struct {
	config          NodeConfig
	leader          *actor.PID
	currentTerm     uint64
	votedFor        *actor.PID
	log             []*actormq.LogEntry
	commitIndex     uint64
	lastApplied     uint64
	votes           uint64
	nodes           map[string]*nodeMetadata
	pendingCommands map[uint64]*commandMetadata
	electionTimer   *time.Timer
	heartbeatTimer  *time.Timer
	status          nodeStatus
}

func NewNode(config NodeConfig) actor.Producer {
	return func() actor.Receiver {
		return &nodeActor{
			config: config,
		}
	}
}

func (node *nodeActor) Receive(act *actor.Context) {
	// log.Printf("%s - %T: %v\n", act.PID().String(), act.Message(), act.Message())
	switch msg := act.Message().(type) {
	case actor.Initialized:
		node.nodes = make(map[string]*nodeMetadata)
		node.pendingCommands = make(map[uint64]*commandMetadata)
		node.electionTimer = time.NewTimer(newElectionTimoutDuration())
		node.heartbeatTimer = time.NewTimer(heartbeatTimeoutDuration)

	case actor.Started:
		act.Send(node.config.DiscoveryPID, &actormq.RegisterNode{})
		act.Send(act.PID(), checkTimers{})

	case *actormq.ActiveNodes:
		node.handleActiveNodes(act, msg)

	case *actor.Ping:
		act.Send(act.Sender(), &actor.Pong{})

	case *actormq.Command:
		node.handleCommand(act, msg)

	case *actormq.AppendEntries:
		node.handleExternalTerm(msg.Term)
		node.handleAppendEntries(act, msg)

	case *actormq.AppendEntriesResult:
		node.handleExternalTerm(msg.Term)
		node.handleAppendEntriesResult(act, msg)

	case *actormq.RequestVote:
		node.handleExternalTerm(msg.Term)
		node.handleRequestVote(act, msg)

	case *actormq.RequestVoteResult:
		node.handleExternalTerm(msg.Term)
		node.handleRequestVoteResult(act, msg)

	case checkTimers:
		select {
		case <-node.heartbeatTimer.C:
			node.heartbeatTimer.Reset(heartbeatTimeoutDuration)
			if pidEquals(node.leader, act.PID()) {
				node.sendAppendEntriesAll(act)
			}
		case <-node.electionTimer.C:
			node.electionTimer.Reset(newElectionTimoutDuration())
			if !pidEquals(act.PID(), node.leader) {
				node.startElection(act)
			}
		default:
		}
		sendWithDelay(act, act.PID(), checkTimers{}, checkTimersInterval)

	}
	node.updateStateMachine(act)
}

func (node *nodeActor) handleActiveNodes(act *actor.Context, msg *actormq.ActiveNodes) {
	node.nodes = make(map[string]*nodeMetadata)
	lastLogIndex, _ := node.lastLogIndexAndTerm()
	for _, pid := range msg.Nodes {
		pid := actormq.PIDToActorPID(pid)
		if !pidEquals(pid, act.PID()) {
			if _, ok := node.nodes[pid.String()]; !ok {
				node.nodes[pid.String()] = &nodeMetadata{
					pid:        pid,
					nextIndex:  lastLogIndex + 1,
					matchIndex: 0,
				}
			}
		}
	}
	node.config.Logger.Info("handleActiveNodes", "msg", msg, "nodes", node.nodes)
}

func (node *nodeActor) handleCommand(act *actor.Context, msg *actormq.Command) {
	if pidEquals(node.leader, act.PID()) {
		node.log = append(node.log, &actormq.LogEntry{
			Command: msg.Command,
			Term:    node.currentTerm,
		})
		newLogIndex := uint64(len(node.log))
		node.pendingCommands[newLogIndex] = &commandMetadata{
			sender: act.Sender(),
		}
		node.sendAppendEntriesAll(act)
	} else {
		act.Send(act.Sender(), &actormq.CommandResult{
			Success:     false,
			RedirectPID: actormq.ActorPIDToPID(node.leader),
		})
	}
}

func (node *nodeActor) handleAppendEntries(act *actor.Context, msg *actormq.AppendEntries) {
	result := &actormq.AppendEntriesResult{}
	defer func() {
		result.PID = actormq.ActorPIDToPID(act.PID())
		result.Term = node.currentTerm
		act.Send(act.Sender(), result)
		node.config.Logger.Info("handleAppendEntries", "msg", msg, "result", result)
	}()

	if msg.Term == node.currentTerm && pidEquals(node.leader, act.PID()) {
		node.config.Logger.Warn("Leader collision!")
	}

	// Condition #1
	// Reply false if term < currentTerm
	if msg.Term < node.currentTerm {
		result.Success = false
		return
	}

	node.leader = actormq.PIDToActorPID(msg.LeaderPID)

	// Condition #2
	// Reply false if log doesn't contain an entry at prevLogIndex whose term matches prevLogTerm
	if msg.PrevLogIndex > 0 {
		if len(node.log) < int(msg.PrevLogIndex) || (len(node.log) > 0 && node.log[msg.PrevLogIndex-1].Term != msg.PrevLogTerm) {
			result.Success = false
			return
		}
	}

	newEntryIndex := msg.PrevLogIndex
	for _, entry := range msg.Entries {
		newEntryIndex++

		// Condition #3
		// If an existing entry conflicts with a new one (same index but different terms),
		// delete the existing entry and all that follow it
		if len(node.log) >= int(newEntryIndex) && node.log[newEntryIndex-1].Term != entry.Term {
			node.log = node.log[:newEntryIndex-1]
		}

		// Condition #4
		// Append any new entries not already in the log
		if len(node.log) < int(newEntryIndex) {
			node.log = append(node.log, entry)
		}
	}

	// Condition #5
	// If leaderCommit > commitIndex,
	// set commitIndex = min(leaderCommit, index of last new entry)
	if msg.LeaderCommit > node.commitIndex {
		node.commitIndex = min(msg.LeaderCommit, newEntryIndex)
	}

	result.Success = true

	if !node.electionTimer.Stop() {
		<-node.electionTimer.C
	}
	node.electionTimer.Reset(newElectionTimoutDuration())
}

func (node *nodeActor) handleAppendEntriesResult(act *actor.Context, msg *actormq.AppendEntriesResult) {
	node.config.Logger.Info("handleAppendEntriesResult", "msg", msg)
	metadata, ok := node.nodes[msg.PID.String()]
	if !ok {
		return
	}
	if msg.Success {
		lastLogIndex, _ := node.lastLogIndexAndTerm()
		metadata.matchIndex = metadata.nextIndex - 1
		metadata.nextIndex = lastLogIndex + 1
	} else {
		if metadata.nextIndex > 1 {
			metadata.nextIndex--
		}
		if err := node.sendAppendEntries(act, metadata.pid); err != nil {
			node.config.Logger.Info("handleAppendEntriesResult", "result", msg, "error", err)
		}
	}
}

func (node *nodeActor) handleRequestVote(act *actor.Context, msg *actormq.RequestVote) {
	result := &actormq.RequestVoteResult{}
	defer func() {
		result.Term = node.currentTerm
		act.Send(act.Sender(), result)
		node.config.Logger.Info("handleRequestVote", "msg", msg, "result", result)
	}()

	// Condition #1
	// Reply false if term < currentTerm
	if msg.Term < node.currentTerm {
		result.VoteGranted = false
		return
	}

	// Condition #2
	// If votedFor is null or candidateId,
	// and candidate's log is at least as up-to-date as receiver's log, grant vote
	candidatePID := actormq.PIDToActorPID(msg.CandidatePID)
	if node.votedFor == nil || node.votedFor.String() == candidatePID.String() {
		if msg.LastLogIndex >= node.lastApplied && (node.lastApplied == 0 || msg.LastLogTerm >= node.log[node.lastApplied-1].Term) {
			node.votedFor = candidatePID
			result.VoteGranted = true
		}
	}
}

func (node *nodeActor) handleRequestVoteResult(act *actor.Context, msg *actormq.RequestVoteResult) {
	node.config.Logger.Info("handleRequestVoteResult", "pid", act.PID(), "sender", act.Sender(), "msg", msg)
	if msg.VoteGranted && msg.Term == node.currentTerm && !pidEquals(node.leader, act.PID()) {
		node.votes++
		if float32(node.votes)/float32(len(node.nodes)) > 0.5 {
			node.config.Logger.Info("Promoted to leader")
			node.leader = act.PID()
			lastLogIndex, _ := node.lastLogIndexAndTerm()
			for _, metadata := range node.nodes {
				metadata.nextIndex = lastLogIndex + 1
				metadata.matchIndex = 0
			}
			node.sendAppendEntriesAll(act)
		}
	}
}

func (node *nodeActor) sendAppendEntriesAll(act *actor.Context) {
	for _, metadata := range node.nodes {
		if err := node.sendAppendEntries(act, metadata.pid); err != nil {
			node.config.Logger.Error("Sending AppendEntries for "+metadata.pid.String(), "error", err.Error())
		}
	}
}

func (node *nodeActor) sendAppendEntries(act *actor.Context, pid *actor.PID) error {
	metadata, ok := node.nodes[pid.String()]
	if !ok {
		return errors.New("server does not exist")
	}

	if metadata.nextIndex == 0 {
		return errors.New("nextIndex is 0 for " + pid.String())
	}

	entries := []*actormq.LogEntry{}
	lastLogIndex, _ := node.lastLogIndexAndTerm()
	if lastLogIndex >= metadata.nextIndex {
		entries = node.log[metadata.nextIndex-1:]
	}

	var prevLogIndex uint64 = metadata.nextIndex - 1
	var prevLogTerm uint64 = 0
	if prevLogIndex > 0 {
		prevLogTerm = node.log[prevLogIndex-1].Term
	}

	act.Send(metadata.pid, &actormq.AppendEntries{
		Term:         node.currentTerm,
		LeaderPID:    actormq.ActorPIDToPID(act.PID()),
		PrevLogTerm:  prevLogTerm,
		PrevLogIndex: prevLogIndex,
		Entries:      entries,
		LeaderCommit: node.commitIndex,
	})
	return nil
}

func (node *nodeActor) startElection(act *actor.Context) {
	defer func() {
		node.config.Logger.Info("Starting election", "term", node.currentTerm)
	}()
	node.currentTerm++
	node.votes = 1
	node.votedFor = act.PID()

	if len(node.nodes)+1 < minServersForElection {
		node.config.Logger.Info("Not enough servers for election")
		return
	}

	lastLogIndex, lastLogTerm := node.lastLogIndexAndTerm()
	for _, metadata := range node.nodes {
		act.Send(metadata.pid, &actormq.RequestVote{
			Term:         node.currentTerm,
			CandidatePID: actormq.ActorPIDToPID(act.PID()),
			LastLogIndex: lastLogIndex,
			LastLogTerm:  lastLogTerm,
		})
	}
}

func (node *nodeActor) lastLogIndexAndTerm() (uint64, uint64) {
	var lastLogIndex uint64 = uint64(len(node.log))
	var lastLogTerm uint64 = 0
	if lastLogIndex > 0 {
		lastLogTerm = node.log[lastLogIndex-1].Term
	}
	return lastLogIndex, lastLogTerm
}

func (node *nodeActor) handleExternalTerm(term uint64) {
	if term > node.currentTerm {
		node.currentTerm = term
		node.leader = nil
		node.votedFor = nil
	}
}

func (node *nodeActor) updateStateMachine(act *actor.Context) {
	if pidEquals(node.leader, act.PID()) {
		for i := uint64(len(node.log)); i >= node.commitIndex+1; i-- {
			if node.log[i-1].Term == node.currentTerm {
				matched := 0
				for _, metadata := range node.nodes {
					if metadata.matchIndex >= i {
						matched++
					}
				}
				if float32(matched) > float32(len(node.nodes))/2 {
					node.commitIndex = i
					break
				}
			}
		}
	}
	for node.commitIndex > node.lastApplied {
		node.lastApplied++
		entry := node.log[node.lastApplied-1]
		if node.config.Handler != nil {
			node.config.Handler(entry.Command)
		}
		command, ok := node.pendingCommands[node.lastApplied]
		if ok {
			act.Send(command.sender, &actormq.CommandResult{
				Success: true,
			})
			delete(node.pendingCommands, node.lastApplied)
		}
		node.config.Logger.Info("Applied command", "index", node.lastApplied, "command", entry.Command)
	}
}
