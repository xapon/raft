package raft

//
// this is an outline of the API that raft must expose to
// the service (or tester). see comments below for
// each of these functions for more details.
//
// rf = Make(...)
//   create a new Raft server.
// rf.Start(command interface{}) (index, term, isleader)
//   start agreement on a new log entry
// rf.GetState() (term, isLeader)
//   ask a Raft for its current term, and whether it thinks it is leader
// ApplyMsg
//   each time a new entry is committed to the log, each Raft peer
//   should send an ApplyMsg to the service (or tester)
//   in the same server.
//

import "sync"
import (
	"../labrpc"
	"fmt"
	"log"
	"time"
)

// how often to send heartbeats
const HEARTBEAT_FREQUENCY = 100 * time.Millisecond

const STATUS_FOLLOWER = 0
const STATUS_CANDIDATE = 1
const STATUS_LEADER = 2

//
// as each Raft peer becomes aware that successive log entries are
// committed, the peer should send an ApplyMsg to the service (or
// tester) on the same server, via the applyCh passed to Make().
//
type ApplyMsg struct {
	Index       int
	Command     interface{}
	UseSnapshot bool   // ignore for lab2; only used in lab3
	Snapshot    []byte // ignore for lab2; only used in lab3
}

//
// this is our representation of Log entry which contains the command
// and  a term when the entry is received by the leader
//
type Log struct {
	Command  interface{}
	Term     int // term when the entry is received by the leader, starts at 1
	Position int // position in the log
}

// A struct generated in a Start to send given Entry to a peer.
// Term is whatever the term leader had when command was issued.
type PeerUpdateCmd struct {
	Entry int
	Term  int
}

//
// A Go object implementing a single Raft peer.
//
type Raft struct {
	mu    sync.Mutex          // Lock to protect shared access to this peer's state
	peers []*labrpc.ClientEnd // RPC end points of all peers
	me    int                 // this peer's index into peers[]

	currentTerm int //This is the term number starting at 1
	votedFor    int //CandidateId that this server voted for in this term
	logEntries  []Log

	// The following variables are volatile states on all servers
	// Both of the following indices increase monotonically and cannot decrease or go back
	commitIndex int // index of highest log entry known to be committed
	lastApplied int // index of the highest log entry applied to state machines

	// The following are leader related properties
	status    int   // status of a raft. 0 means follower, 1 means candidate, 2 means leader
	nextIndex []int // for each server, index of the next log entry to send to that server
	// initialized to leader's last log index + 1
	matchIndex []int // for each server, index of highest log entry known to be replicated on that server
	// initialized to zero, increases monotonically

	// This keeps track of peers that we're currently sending entries to.
	// If value is true, we won't send this peer a heartbeat.
	updatingPeers []bool

	// A queue of new entries for each peer
	peerUpdates []chan PeerUpdateCmd

	electionTimer  *time.Timer
	heartbeatTimer *time.Timer

	// this channel serves as a buffer to send committed entries to
	// before they get to a client
	commitCh chan ApplyMsg
	// message channel to client
	clientCh chan ApplyMsg
}

// return currentTerm and whether this server
// believes it is the leader.
func (rf *Raft) GetState() (int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return rf.currentTerm, rf.status == STATUS_LEADER
}

// example AppendEntriesRPC arguments structure
type AppendEntriesArgs struct {
	Term              int   // term number
	LeaderId          int   // id of the leader
	PrevLogIndex      int   // index of the log immediately preceding new ones
	PrevLogTerm       int   // term of prevLogIndex entry
	LogEntries        []Log //log entries to store. For heartbeat, this is empty. May send more than one for efficiency
	LeaderCommitIndex int
}

// example AppendEntriesRPC reply structure
type AppendEntriesReply struct {
	Term      int  // term number
	Success   bool //true if follower contains log entry matching PrevLogIndex and PrevLogTerm
	PeerIndex int  // index of the raft instance in leader's nextIndex slice
	NextIndex int  // Updated nextIndex for the peer
}

//
// example AppendEntries RPC handler.
//
func (rf *Raft) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	rf.becomeFollowerIfTermIsOlderOrEqual(args.Term, fmt.Sprintf("AppendEntries request from %d", args.LeaderId))

	if args.Term < rf.currentTerm { // This happens when an old failed leader just woke up
		reply.Success = false
		rf.DPrintf("Got AppendEntries from %d, failing because RPC term %d is old", args.LeaderId, args.Term)
	} else {
		rf.resetElectionTimer()
		// check if we have log consistency
		if args.PrevLogIndex >= len(rf.logEntries) {
			reply.Success = false
			rf.DPrintf(
				"AppendEntries rejected because RPC prevLogIndex is >= host logEntries length",
			)
		} else if args.PrevLogTerm > 0 && args.PrevLogIndex > -1 && args.PrevLogTerm != rf.logEntries[args.PrevLogIndex].Term {
			reply.Success = false
			rf.DPrintf(
				"AppendEntries rejected because RPC prevLogIndex does not match host prevLogEntry term",
			)
		} else {
			reply.Success = true

			// Delete any inconsistent log entries
			if args.PrevLogIndex > -1 {
				rf.logEntries = rf.logEntries[0: args.PrevLogIndex+1]
				rf.lastApplied = len(rf.logEntries) - 1
				reply.NextIndex = len(rf.logEntries) - 1
			}

			if len(args.LogEntries) > 0 {
				// append leader's log to its own logs
				rf.logEntries = append(rf.logEntries, args.LogEntries...)
				rf.lastApplied = len(rf.logEntries) - 1
				reply.NextIndex = len(rf.logEntries) - 1
				rf.DPrintf(
					"AppendEntries applied from %d, leader term %d, prev log index %d, next index %d, %d new entries added, my entries len %d. Leader ci %d, my ci %d",
					args.LeaderId,
					args.Term,
					args.PrevLogIndex,
					reply.NextIndex,
					len(args.LogEntries),
					len(rf.logEntries),
					args.LeaderCommitIndex,
					rf.commitIndex,
				)
			}
		}
	}

	reply.Term = rf.currentTerm

	// Decide if we need to send client commit message
	if reply.Success && args.LeaderCommitIndex > rf.commitIndex {
		oldCommitIndex := rf.commitIndex + 1
		rf.commitIndex = min(args.LeaderCommitIndex, len(rf.logEntries)-1)

		for oldCommitIndex <= rf.commitIndex {
			if oldCommitIndex >= 0 {
				// NOTE TODO: Normally, we will send index in our slice/array. However, log entries in actual raft
				// NOTE TODO: starts at 1 instead of 0. So, we need to increment the index by one
				cmdToSend := rf.logEntries[oldCommitIndex].Command
				rf.commitCh <- ApplyMsg{
					Index:   oldCommitIndex + 1,
					Command: cmdToSend,
				}
			}
			oldCommitIndex++
		}
	}
}

// example RequestVote RPC arguments structure.
// field names must start with capital letters!
//
type RequestVoteArgs struct {
	Term         int // this is the term number of the election
	CandidateId  int // id of candidate requesting the vote
	LastLogIndex int // index of the candidate's last log entry
	LastLogTerm  int // term number of the candidate's last log entry
}

//
// example RequestVote RPC reply structure.
// field names must start with capital letters!
//
type RequestVoteReply struct {
	Term        int  // term number of the election
	VoteGranted bool // If the vote is granted
}

//
// RequestVote RPC handler.
//
func (rf *Raft) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	rf.becomeFollowerIfTermIsOlder(args.Term, fmt.Sprintf("RequestVote request from %d", args.CandidateId))

	reply.Term = rf.currentTerm
	reply.VoteGranted = false

	if rf.votedFor == -1 { // first check to grant vote is that raft has yet to vote in the term
		selfLastLogTerm := 0
		if len(rf.logEntries) > 0 {
			selfLastLogTerm = rf.logEntries[len(rf.logEntries)-1].Term
		}
		if selfLastLogTerm < args.LastLogTerm { // If a new term starts, grant the vote
			reply.VoteGranted = true
			rf.votedFor = args.CandidateId
			rf.DPrintf(
				"granting vote to %d because my last log term is less than candidate's",
				args.CandidateId)

		} else if selfLastLogTerm == args.LastLogTerm { // if in the same term, whoever has longer log is more up-to-date
			if len(rf.logEntries) <= args.LastLogIndex+1 {
				reply.VoteGranted = true
				rf.votedFor = args.CandidateId

				rf.DPrintf(
					"granting vote to %d because candidate has >= log entries: my %d, its %d",
					args.CandidateId,
					len(rf.logEntries),
					args.LastLogIndex+1,
				)
			}
		}
	}

	if reply.VoteGranted {
		rf.resetElectionTimer()
	}

	rf.DPrintf(
		"received vote request from %d, granted: %t",
		args.CandidateId, reply.VoteGranted)
}

//
// example code to send a RequestVote RPC to a server.
// server is the index of the target server in rf.peers[].
// expects RPC arguments in args.
// fills in *reply with RPC reply, so caller should
// pass &reply.
// the types of the args and reply passed to Call() must be
// the same as the types of the arguments declared in the
// handler function (including whether they are pointers).
//
// The labrpc package simulates a lossy network, in which servers
// may be unreachable, and in which requests and replies may be lost.
// Call() sends a request and waits for a reply. If a reply arrives
// within a timeout interval, Call() returns true; otherwise
// Call() returns false. Thus Call() may not return for a while.
// A false return can be caused by a dead server, a live server that
// can't be reached, a lost request, or a lost reply.
//
// Call() is guaranteed to return (perhaps after a delay) *except* if the
// handler function on the server side does not return.  Thus there
// is no need to implement your own timeouts around Call().
//
// look at the comments in ../labrpc/labrpc.go for more details.
//
// if you're having trouble getting RPC to work, check that you've
// capitalized all field names in structs passed over RPC, and
// that the caller passes the address of the reply struct with &, not
// the struct itself.
//
func (rf *Raft) sendRequestVote(server int, args *RequestVoteArgs, reply *RequestVoteReply) bool {
	ok := rf.peers[server].Call("Raft.RequestVote", args, reply)
	return ok
}

// Send RequestVote to all peers, collect results and become a leader if got a majority of votes
func (rf *Raft) requestVoteFromPeers() {
	lastLogTerm := 0
	lastLogIndex := -1
	rf.mu.Lock()
	if len(rf.logEntries) > 0 {
		lastLogIndex = len(rf.logEntries) - 1
		lastLogTerm = rf.logEntries[lastLogIndex].Term
	}
	args := RequestVoteArgs{
		Term:         rf.currentTerm,
		CandidateId:  rf.me,
		LastLogTerm:  lastLogTerm,
		LastLogIndex: lastLogIndex,
	}
	startTerm := rf.currentTerm
	rf.mu.Unlock()

	// to send response structure and "ok" flag in a channel,
	// we need to wrap it in a structure
	type ResponseMsg struct {
		RequestVoteReply
		IsOk      bool
		PeerIndex int
	}

	responseChan := make(chan ResponseMsg)
	rf.DPrintf("sending RequestVote")
	// received response counter and expected number of responses
	rxCount := 0
	expectedRxCount := len(rf.peers) - 1

	// send requests concurrently
	for i, _ := range rf.peers {
		if i == rf.me {
			continue
		}

		go func(peerIndex int) {
			resp := RequestVoteReply{}
			ok := rf.sendRequestVote(peerIndex, &args, &resp)
			responseChan <- ResponseMsg{
				resp,
				ok,
				peerIndex,
			}
		}(i)
	}

	grantedVoteCount := 1 // initial vote is a vote for self

	// collect responses
	for resp := range responseChan {
		rf.mu.Lock()
		rf.DPrintf(
			"received RequestVote response from %d, is ok: %t, granted: %t, start term: %d",
			resp.PeerIndex,
			resp.IsOk,
			resp.VoteGranted,
			startTerm,
		)

		if rf.currentTerm != startTerm {
			rf.DPrintf(
				"got RequestVote result, but term has already changed, ignoring it",
			)
		} else if resp.IsOk && resp.VoteGranted {
			grantedVoteCount++
			// if enough responses received, become a leader
			// - don't need to wait for other responses
			if grantedVoteCount == rf.getMajoritySize() {
				if rf.status == STATUS_CANDIDATE {
					rf.BecomeLeader()
				} else {
					// this might happen when votes from some older term are received,
					// but this host is not a candidate any more, so we ignore it
					rf.DPrintf("got votes, but host is not a candidate")
				}
			}
		} else if resp.IsOk {
			rf.becomeFollowerIfTermIsOlder(resp.Term, "RequestVotes response")
		}

		rf.mu.Unlock()
		rxCount++

		if rxCount == expectedRxCount {
			return
		}
	}
}

// Send AppendEntries to given peer
func (rf *Raft) sendAppendEntries(server int, args *AppendEntriesArgs, reply *AppendEntriesReply) bool {
	ok := rf.peers[server].Call("Raft.AppendEntries", args, reply)
	return ok
}

// Send AppendEntries to all peers and collect results
func (rf *Raft) broadcastHeartbeats() {
	rf.mu.Lock()
	peersToSend := []int{}
	// prepare arguments
	peerArgs := []AppendEntriesArgs{}
	for i, _ := range rf.peers {
		// skip myself and peers that may be already receiving
		// non-empty AppendEntries from "updatePeer"
		if i == rf.me || rf.updatingPeers[i] == true {
			continue
		}

		peersToSend = append(peersToSend, i)
		args := AppendEntriesArgs{
			Term:              rf.currentTerm,
			LeaderId:          rf.me,
			LeaderCommitIndex: rf.commitIndex,
			LogEntries:        []Log{},
			PrevLogIndex:      rf.nextIndex[i],
			PrevLogTerm:       -1,
		}

		if args.PrevLogIndex >= 0 {
			args.PrevLogTerm = rf.logEntries[args.PrevLogIndex].Term
		}
		peerArgs = append(peerArgs, args)
	}
	startTerm := rf.currentTerm
	rf.mu.Unlock()

	if len(peersToSend) == 0 {
		return
	}
	// received response counter and expected number of responses
	rxCount := 0
	expectedRxCount := len(peersToSend)

	// to send response structure and "ok" flag in a channel,
	// we need to wrap it in a structure
	type ResponseMsg struct {
		AppendEntriesReply
		IsNetworkOK bool
		Peer        int
		// for debugging purposes - to see how delayed the response was
		DateSent time.Time
	}
	responseChan := make(chan ResponseMsg)

	if DebugHeartbeats > 0 {
		rf.DPrintf("sending heartbeats")
	}

	// send requests concurrently
	for i, peerIndex := range peersToSend {
		go func(peerIndex int, args AppendEntriesArgs) {
			resp := AppendEntriesReply{PeerIndex: peerIndex}
			dateSent := time.Now()
			ok := rf.sendAppendEntries(peerIndex, &args, &resp)
			responseChan <- ResponseMsg{
				resp,
				ok,
				peerIndex,
				dateSent,
			}
		}(peerIndex, peerArgs[i])
	}

	// collect responses
	for resp := range responseChan {
		if DebugHeartbeats > 0 {
			rf.DPrintf(
				"received heartbeat response from %d, ok: %t, success: %t, sent at: %s",
				resp.Peer,
				resp.IsNetworkOK,
				resp.Success,
				resp.DateSent.Format(time.StampMicro),
			)
		}

		rf.mu.Lock()

		if resp.IsNetworkOK {
			// this happens when we just woke up as a previous leader
			rf.becomeFollowerIfTermIsOlder(resp.Term, "heartbeat response")

			if rf.status == STATUS_LEADER && !resp.Success && startTerm == rf.currentTerm {
				rf.DPrintf(
					"\tUpdating follower %d after heartbeat response to cmd index=%d v=%+v",
					resp.PeerIndex,
					rf.lastApplied,
					rf.logEntries[rf.lastApplied].Command,
				)
				rf.peerUpdates[resp.PeerIndex] <- PeerUpdateCmd{rf.lastApplied, rf.currentTerm}
			}
		}

		rf.mu.Unlock()

		rxCount++
		if rxCount == expectedRxCount {
			return
		}
	}
}

// Debug print function,
// that prints current host info automatically
func (rf *Raft) DPrintf(format string, a ...interface{}) {
	if Debug == 0 {
		return
	}

	var lastAppliedCmd interface{}
	var lastCommittedCmd interface{}

	if rf.lastApplied >= 0 {
		lastAppliedCmd = rf.logEntries[rf.lastApplied].Command
	}

	if rf.commitIndex >= 0 {
		lastCommittedCmd = rf.logEntries[rf.commitIndex].Command
	}

	args := make([]interface{}, 0, 8+len(a))
	args = append(
		append(
			args,
			rf.me,
			rf.status,
			rf.currentTerm,
			len(rf.logEntries),
			rf.commitIndex,
			lastCommittedCmd,
			rf.lastApplied,
			lastAppliedCmd,
		),
		a...,
	)
	// Log format:
	// [host_index status term log_length committed_cmd_index/value applied_cmd_index/value]
	log.Printf("\t[i%d s%d t%d l%d cmd-comm:%d/%+v cmd-app:%d/%v] "+format, args...)
}

//
// the service using Raft (e.g. a k/v server) wants to start
// agreement on the next command to be appended to Raft's log. if this
// server isn't the leader, returns false. otherwise start the
// agreement and return immediately. there is no guarantee that this
// command will ever be committed to the Raft log, since the leader
// may fail or lose an election.
//
// the first return value is the index that the command will appear at
// if it's ever committed. the second return value is the current
// term. the third return value is true if this server believes it is
// the leader.
//
func (rf *Raft) Start(command interface{}) (int, int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	if rf.status != STATUS_LEADER {
		return -1, -1, false
	}

	newLog := Log{Command: command, Term: rf.currentTerm, Position: len(rf.logEntries)}
	rf.logEntries = append(rf.logEntries, newLog)
	rf.nextIndex[rf.me] = len(rf.logEntries) - 1
	rf.matchIndex[rf.me] = rf.nextIndex[rf.me]
	newLength := len(rf.logEntries)
	rf.lastApplied = len(rf.logEntries) - 1

	rf.DPrintf("\tEnqueueing new command: %+v", command)
	rf.enqueueEntryBroadcast(
		PeerUpdateCmd{rf.lastApplied, rf.currentTerm},
	)

	return newLength, rf.currentTerm, true
}

// Prepares arguments for AppendEntries RPC
func (rf *Raft) constructArgsForBroadcast(peerIndex int, maxEntryIndex int) AppendEntriesArgs {
	prevLogTerm := 0
	prevLogIndex := rf.nextIndex[peerIndex]
	if prevLogIndex >= 0 {
		prevLogTerm = rf.logEntries[prevLogIndex].Term
	}

	var entriesToSend []Log

	if prevLogIndex+1 <= maxEntryIndex+1 {
		entriesToSend = rf.logEntries[prevLogIndex+1: maxEntryIndex+1]
	} else {
		// when we want to send follower an entry that was already accepted by it
		// - no need to do anything.
		// TODO this scenario can be omptimized
	}
	args := AppendEntriesArgs{
		Term:              rf.currentTerm,
		LeaderId:          rf.me,
		PrevLogIndex:      prevLogIndex,
		PrevLogTerm:       prevLogTerm,
		LogEntries:        entriesToSend,
		LeaderCommitIndex: rf.commitIndex,
	}

	return args
}

// Enqueue AppendEntries command for all peers
func (rf *Raft) enqueueEntryBroadcast(cmd PeerUpdateCmd) {
	if rf.status != STATUS_LEADER {
		rf.DPrintf(
			"skipping AppendEntries because host is not a leader",
		)
		return
	}

	rf.DPrintf(
		"enqueueing AppendEntries for entry %d, cmd %+v",
		cmd.Entry,
		rf.logEntries[cmd.Entry].Command,
	)

	// Send new entry to each peer.
	for i, _ := range rf.peers {
		if i == rf.me {
			continue
		}

		rf.peerUpdates[i] <- cmd
	}
}

// Sends entries to peers, as they appear in the update channel
func (rf *Raft) updatePeersInBackground() {
	for i, _ := range rf.peerUpdates {
		go func(peer int) {
			for cmd := range rf.peerUpdates[peer] {
				rf.updatePeer(peer, cmd)
			}
		}(i)
	}
}

// Sends committed commands to client channel
func (rf *Raft) commitInBackground() {
	for msg := range rf.commitCh {
		rf.DPrintf(
			"\tCommitting cmd %+v with index %d",
			msg.Command,
			msg.Index,
		)
		rf.clientCh <- msg
	}
}

// Sends AppendEntries to peer until its index becomes >= entryIndex
func (rf *Raft) updatePeer(peer int, cmd PeerUpdateCmd) {
	retries := 0
	rf.mu.Lock()
	rf.updatingPeers[peer] = true

	// upon returning from this function,
	// mark a peer as not receiving updates,
	// and unlock the mutex
	defer func() {
		rf.updatingPeers[peer] = false
		rf.mu.Unlock()
	}()

	for {
		if rf.status != STATUS_LEADER {
			return
		}

		resp := AppendEntriesReply{PeerIndex: peer}
		args := rf.constructArgsForBroadcast(resp.PeerIndex, cmd.Entry)
		rf.DPrintf(
			"Sending AppendEntries to %d with %d entries",
			resp.PeerIndex,
			len(args.LogEntries),
		)
		rf.mu.Unlock()

		ok := rf.sendAppendEntries(peer, &args, &resp)
		rf.mu.Lock()

		if ok {
			// this happens when we just woke up as a previous leader
			rf.becomeFollowerIfTermIsOlder(resp.Term, "AppendEntries response")
		}

		if rf.status != STATUS_LEADER {
			rf.DPrintf(
				"Exiting UpdatePeer because host is not a leader any more",
			)
			return
		}

		if ok && resp.Success {
			// Update the match index and next index for this particular follower
			rf.nextIndex[resp.PeerIndex] = resp.NextIndex
			rf.matchIndex[resp.PeerIndex] = rf.nextIndex[resp.PeerIndex]
			rf.DPrintf(
				"AppendEntries to host %d succeeded with entry index %d; next index: %d, ",
				resp.PeerIndex,
				cmd.Entry,
				rf.nextIndex[resp.PeerIndex],
			)

			if resp.NextIndex >= cmd.Entry {
				rf.updatingPeers[peer] = false
				successCount := 0
				for i, _ := range rf.peers {
					if rf.matchIndex[i] >= cmd.Entry {
						successCount++
					}
				}

				rf.DPrintf("Success count for entry %d: %d", cmd.Entry, successCount)
				if successCount >= rf.getMajoritySize() {
					// commit only if entry wasn't already committed
					if rf.commitIndex == cmd.Entry-1 {
						rf.commitCh <- ApplyMsg{
							Index:   cmd.Entry + 1,
							Command: rf.logEntries[cmd.Entry].Command,
						}
						rf.commitIndex = cmd.Entry
					}
					return
				} else {
					// this peer accepted an entry, but there is no majority yet
					return
				}
			}
		} else if ok && !resp.Success {
			// If it's a log consistency failure, we need to decrement nextIndex for the particular follower and resend log entry
			rf.nextIndex[resp.PeerIndex] = rf.nextIndex[resp.PeerIndex] - 1

			rf.DPrintf(
				"Decremented nextIndex for peer %d: %d",
				resp.PeerIndex,
				rf.nextIndex[resp.PeerIndex],
			)
		}

		retries++
		rf.DPrintf(
			"\tRetrying AppendEntries to host %d with cmd %+v; network is ok: %t, next index: %d [%d retries]",
			resp.PeerIndex,
			rf.logEntries[rf.nextIndex[resp.PeerIndex]].Command,
			ok,
			rf.nextIndex[resp.PeerIndex],
			retries,
		)
	}
}

//
// the tester calls Kill() when a Raft instance won't
// be needed again. you are not required to do anything
// in Kill(), but it might be convenient to (for example)
// turn off debug output from this instance.
//
func (rf *Raft) Kill() {
	// Your code here, if desired.
}

// Turns current host into leader
func (rf *Raft) BecomeLeader() {
	if rf.status != STATUS_LEADER {
		rf.status = STATUS_LEADER
	}

	rf.votedFor = -1

	if !rf.electionTimer.Stop() {
		<-rf.electionTimer.C
	}
	rf.DPrintf("server %d becomes a new leader with log entry length %d", rf.me, len(rf.logEntries))

	/* Initialize all nextIndex values to the next Index the leader will send to followers
	And the nextIndex the leader will send to a follower is the index of the latest known replicated entry
	so that the follower can use the index to check against its own log */
	rf.nextIndex = make([]int, len(rf.peers))
	for index, _ := range rf.peers {
		rf.nextIndex[index] = len(rf.logEntries) - 1
	}

	/* Initialize all matchIndex values for all the peers. This is the index of the highest log entry
	known to replicated on server. Upon leader election, all matchIndex initialized to zero
	*/
	rf.matchIndex = make([]int, len(rf.peers))
	for index, _ := range rf.peers {
		if index == rf.me {
			rf.matchIndex[rf.me] = len(rf.logEntries) - 1
		} else {
			rf.matchIndex[index] = -1
		}
	}

	for i := range rf.peers {
		rf.updatingPeers[i] = false
	}

	// send heartbeat immediately without waiting for a ticker
	// to make sure other peers will not timeout.
	go rf.broadcastHeartbeats()
}

// Turns current host into candidate
func (rf *Raft) BecomeCandidate() {
	rf.status = STATUS_CANDIDATE
	rf.currentTerm++
	rf.DPrintf("start leader election with term %d server %d", rf.currentTerm, rf.me)
	rf.votedFor = rf.me
}

// Turns current host into follower during election because either we discovered the current leader or a new turn
// Returns new host status.
// Comment is used only for debug.
func (rf *Raft) becomeFollowerIfTermIsOlder(term int, comment string) int {
	// check if we have a new term
	if rf.currentTerm < term {
		return rf.becomeFollower(term, comment)
	}

	return rf.status
}

// Turns current host into follower and updates its term, if given term is newer.
// Returns new host status.
// Comment is used only for debug.
func (rf *Raft) becomeFollowerIfTermIsOlderOrEqual(term int, comment string) int {
	if rf.currentTerm <= term {
		return rf.becomeFollower(term, comment)
	}

	return rf.status
}

// Turns current host into follower.
// Returns new host status.
func (rf *Raft) becomeFollower(newTerm int, comment string) int {
	statusUpdated := false
	termUpdated := false
	oldTerm := rf.currentTerm

	if rf.status != STATUS_FOLLOWER {
		rf.status = STATUS_FOLLOWER
		statusUpdated = true
	}

	rf.votedFor = -1

	if rf.currentTerm != newTerm {
		rf.currentTerm = newTerm
		termUpdated = true
	}

	// this is just for debugging
	if statusUpdated && termUpdated {
		rf.DPrintf(
			"[%s] term updated, old: %d. Host is now a follower.",
			comment, oldTerm)
	} else if statusUpdated {
		rf.DPrintf(
			"[%s] host is now a follower.",
			comment)
	} else if termUpdated {
		rf.DPrintf(
			"[%s] term updated, old: %d",
			comment, oldTerm)
	}

	return rf.status
}

func (rf *Raft) resetElectionTimer() {
	rf.electionTimer.Reset(getElectionTimeout())
}

func (rf *Raft) resetHeartbeatTimer() {
	rf.heartbeatTimer.Reset(HEARTBEAT_FREQUENCY)
}

// Returns the number of hosts that forms a majority
func (rf *Raft) getMajoritySize() int {
	return len(rf.peers)/2 + 1
}

// Processes timers for election (if follower) and heartbeats (if leader)
func (rf *Raft) runTimers() {
	for {
		select {
		case <-rf.electionTimer.C:
			rf.mu.Lock()
			// time to initiate an election
			rf.DPrintf("election timeout")
			rf.BecomeCandidate()
			rf.resetElectionTimer()
			rf.mu.Unlock()
			go rf.requestVoteFromPeers()
			break
		case <-rf.heartbeatTimer.C:
			rf.mu.Lock()
			// time to send a heartbeat
			if rf.status == STATUS_LEADER {
				go rf.broadcastHeartbeats()
			}
			rf.resetHeartbeatTimer()
			rf.mu.Unlock()
			break
		}
	}
}

//
// the service or tester wants to create a Raft server. the ports
// of all the Raft servers (including this one) are in peers[]. this
// server's port is peers[me]. all the servers' peers[] arrays
// have the same order. applyCh is a channel on which the
// tester or service expects Raft to send ApplyMsg messages.
// Make() must return quickly, so it should start goroutines
// for any long-running work.
//
func Make(peers []*labrpc.ClientEnd, me int, applyCh chan ApplyMsg) *Raft {
	rf := &Raft{}
	log.SetFlags(log.Lmicroseconds)
	rf.peers = peers
	rf.me = me
	rf.status = STATUS_FOLLOWER
	rf.logEntries = []Log{}
	rf.commitIndex = -1
	rf.lastApplied = -1
	rf.votedFor = -1
	rf.electionTimer = time.NewTimer(getElectionTimeout())
	rf.heartbeatTimer = time.NewTimer(HEARTBEAT_FREQUENCY)
	rf.clientCh = applyCh
	rf.updatingPeers = make([]bool, len(rf.peers))
	rf.peerUpdates = make([]chan PeerUpdateCmd, len(rf.peers))
	// we don't want this channel to block, so we set a large enough buffer size
	rf.commitCh = make(chan ApplyMsg, 100)

	for i, _ := range rf.peers {
		rf.updatingPeers[i] = false
		// we don't want these channels to block when sending to them,
		// so we set a safe, large enough buffer size
		rf.peerUpdates[i] = make(chan PeerUpdateCmd, 500)
	}

	rf.DPrintf("Majority size: %d", rf.getMajoritySize())

	go rf.runTimers()
	rf.updatePeersInBackground()
	go rf.commitInBackground()

	return rf
}
