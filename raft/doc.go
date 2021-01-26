// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/*
Package raft sends and receives messages in the Protocol Buffer format
defined in the raftpb package.

raft 以协议缓冲区格式发送和接收消息
在raftpb软件包中定义。

Raft is a protocol with which a cluster of nodes can maintain a replicated state machine.
The state machine is kept in sync through the use of a replicated log.

Raft是一种协议，通过它，节点集群可以维护一个复制的状态机。
状态机通过使用复制的日志保持同步。

For more details on Raft, see "In Search of an Understandable Consensus Algorithm"
(https://raft.github.io/raft.pdf) by Diego Ongaro and John Ousterhout.


A simple example application, _raftexample_, is also available to help illustrate
how to use this package in practice:
https://github.com/etcd-io/etcd/tree/master/contrib/raftexample

Usage
用法

The primary object in raft is a Node. You either start a Node from scratch
using raft.StartNode or start a Node from some initial state using raft.RestartNode.

raft中的主要对象是节点。要么从头开始创建节点
使用 raft 开始节点或者使用raft.RestartNode文件.

To start a node from scratch:
从头开始节点：

  storage := raft.NewMemoryStorage()
  c := &Config{
    ID:              0x01,
    ElectionTick:    10,
    HeartbeatTick:   1,
    Storage:         storage,
    MaxSizePerMsg:   4096,
    MaxInflightMsgs: 256,
  }
  n := raft.StartNode(c, []raft.Peer{{ID: 0x02}, {ID: 0x03}})

To restart a node from previous state:
要从以前的状态重新启动节点，请执行以下操作：

  storage := raft.NewMemoryStorage()

  // recover the in-memory storage from persistent
  // snapshot, state and entries.
  storage.ApplySnapshot(snapshot)
  storage.SetHardState(state)
  storage.Append(entries)

  c := &Config{
    ID:              0x01,
    ElectionTick:    10,
    HeartbeatTick:   1,
    Storage:         storage,
    MaxSizePerMsg:   4096,
    MaxInflightMsgs: 256,
  }

  // restart raft without peer information.
  // peer information is already included in the storage.
  n := raft.RestartNode(c)

Now that you are holding onto a Node you have a few responsibilities:
现在您已经拥有一个节点，您将承担以下几项责任：

First, you must read from the Node.Ready() channel and process the updates
it contains. These steps may be performed in parallel, except as noted in step
首先，您必须从Node.Ready（）通道读取并处理更新
它包含。这些步骤可以并行执行，除非步骤中另有说明

2.

1. Write HardState, Entries, and Snapshot to persistent storage if they are
not empty. Note that when writing an Entry with Index i, any
previously-persisted entries with Index >= i must be discarded.

1.将HardState，Entries和Snapshot写入持久性存储（如果它们是
不是空的。请注意，使用索引i编写条目时，任何
必须舍弃索引> = i的先前持久的条目。

2. Send all Messages to the nodes named in the To field. It is important that
no messages be sent until the latest HardState has been persisted to disk,
and all Entries written by any previous Ready batch (Messages may be sent while
entries from the same batch are being persisted). To reduce the I/O latency, an
optimization can be applied to make leader write to disk in parallel with its
followers (as explained at section 10.2.1 in Raft thesis). If any Message has type
MsgSnap, call Node.ReportSnapshot() after it has been sent (these messages may be
large).

2.将所有消息发送到“收件人”字段中命名的节点。重要的是
在将最新的HardState持久化到磁盘之前，不会发送任何消息，
以及任何以前的“就绪”批次编写的所有条目（消息可能会在
来自同一批次的条目将被保留）。为了减少I / O延迟，
可以应用优化使领导者与其磁盘并行地写入磁盘
追随者（如Raft论文中的10.2.1节所述）。如果任何消息具有类型
MsgSnap，发送后调用Node.ReportSnapshot（）（这些消息可能是
大）。

Note: Marshalling messages is not thread-safe; it is important that you
make sure that no new entries are persisted while marshalling.
The easiest way to achieve this is to serialize the messages directly inside
your main raft loop.

注意：编组消息不是线程安全的；重要的是你
确保在编组时不保留任何新条目。
最简单的方法是直接在内部序列化消息
你的主要筏圈。


3. Apply Snapshot (if any) and CommittedEntries to the state machine.
If any committed Entry has Type EntryConfChange, call Node.ApplyConfChange()
to apply it to the node. The configuration change may be cancelled at this point
by setting the NodeID field to zero before calling ApplyConfChange
(but ApplyConfChange must be called one way or the other, and the decision to cancel
must be based solely on the state machine and not external information such as
the observed health of the node).

3.将快照（如果有）和CommittedEntries应用于状态机。
如果任何已提交的条目的类型为EntryConfChange，则调用Node.ApplyConfChange（）
将其应用于节点。此时可能会取消配置更改
通过在调用ApplyConfChange之前将NodeID字段设置为零
（但是ApplyConfChange必须以一种或另一种方式调用，并决定取消
必须仅基于状态机，而不是外部信息，例如
观察到的节点运行状况）。

4. Call Node.Advance() to signal readiness for the next batch of updates.
This may be done at any time after step 1, although all updates must be processed
in the order they were returned by Ready.

4.调用Node.Advance（）表示已准备好进行下一批更新。
尽管必须处理所有更新，但这可以在步骤1之后的任何时间完成。
按Ready退回的顺序。

Second, all persisted log entries must be made available via an
implementation of the Storage interface. The provided MemoryStorage
type can be used for this (if you repopulate its state upon a
restart), or you can supply your own disk-backed implementation.

其次，所有持久化的日志条目必须通过
存储接口的实现。提供的MemoryStorage
类型可用于此目的（如果您在
重新启动），也可以提供自己的磁盘支持的实施。

Third, when you receive a message from another node, pass it to Node.Step:

第三，当您从另一个节点收到消息时，将其传递给Node.Step：

	func recvRaftRPC(ctx context.Context, m raftpb.Message) {
		n.Step(ctx, m)
	}

Finally, you need to call Node.Tick() at regular intervals (probably
via a time.Ticker). Raft has two important timeouts: heartbeat and the
election timeout. However, internally to the raft package time is
represented by an abstract "tick".

最后，您需要定期调用Node.Tick（）（可能
通过时间。Raft 有两个重要的超时：心跳和
选举超时。但是，内部到 Raft 包装的时间是
用抽象的“刻度”表示。

The total state machine handling loop will look something like this:

总的状态机处理循环将如下所示：

  for {
    select {
    case <-s.Ticker:
      n.Tick()
    case rd := <-s.Node.Ready():
      saveToStorage(rd.State, rd.Entries, rd.Snapshot)
      send(rd.Messages)
      if !raft.IsEmptySnap(rd.Snapshot) {
        processSnapshot(rd.Snapshot)
      }
      for _, entry := range rd.CommittedEntries {
        process(entry)
        if entry.Type == raftpb.EntryConfChange {
          var cc raftpb.ConfChange
          cc.Unmarshal(entry.Data)
          s.Node.ApplyConfChange(cc)
        }
      }
      s.Node.Advance()
    case <-s.done:
      return
    }
  }

To propose changes to the state machine from your node take your application
data, serialize it into a byte slice and call:

要从您的节点提出对状态机的更改建议，请使用您的应用程序
数据，将其序列化为字节片并调用：

	n.Propose(ctx, data)

If the proposal is committed, data will appear in committed entries with type
raftpb.EntryNormal. There is no guarantee that a proposed command will be
committed; you may have to re-propose after a timeout.

To add or remove a node in a cluster, build ConfChange struct 'cc' and call:

	n.ProposeConfChange(ctx, cc)

After config change is committed, some committed entry with type
raftpb.EntryConfChange will be returned. You must apply it to node through:

	var cc raftpb.ConfChange
	cc.Unmarshal(data)
	n.ApplyConfChange(cc)

Note: An ID represents a unique node in a cluster for all time. A
given ID MUST be used only once even if the old node has been removed.
This means that for example IP addresses make poor node IDs since they
may be reused. Node IDs must be non-zero.

Implementation notes

This implementation is up to date with the final Raft thesis
(https://github.com/ongardie/dissertation/blob/master/stanford.pdf), although our
implementation of the membership change protocol differs somewhat from
that described in chapter 4. The key invariant that membership changes
happen one node at a time is preserved, but in our implementation the
membership change takes effect when its entry is applied, not when it
is added to the log (so the entry is committed under the old
membership instead of the new). This is equivalent in terms of safety,
since the old and new configurations are guaranteed to overlap.

To ensure that we do not attempt to commit two membership changes at
once by matching log positions (which would be unsafe since they
should have different quorum requirements), we simply disallow any
proposed membership change while any uncommitted change appears in
the leader's log.

This approach introduces a problem when you try to remove a member
from a two-member cluster: If one of the members dies before the
other one receives the commit of the confchange entry, then the member
cannot be removed any more since the cluster cannot make progress.
For this reason it is highly recommended to use three or more nodes in
every cluster.

MessageType

Package raft sends and receives message in Protocol Buffer format (defined
in raftpb package). Each state (follower, candidate, leader) implements its
own 'step' method ('stepFollower', 'stepCandidate', 'stepLeader') when
advancing with the given raftpb.Message. Each step is determined by its
raftpb.MessageType. Note that every step is checked by one common method
'Step' that safety-checks the terms of node and incoming message to prevent
stale log entries:

	'MsgHup' is used for election. If a node is a follower or candidate, the
	'tick' function in 'raft' struct is set as 'tickElection'. If a follower or
	candidate has not received any heartbeat before the election timeout, it
	passes 'MsgHup' to its Step method and becomes (or remains) a candidate to
	start a new election.

	'MsgBeat' is an internal type that signals the leader to send a heartbeat of
	the 'MsgHeartbeat' type. If a node is a leader, the 'tick' function in
	the 'raft' struct is set as 'tickHeartbeat', and triggers the leader to
	send periodic 'MsgHeartbeat' messages to its followers.

	'MsgProp' proposes to append data to its log entries. This is a special
	type to redirect proposals to leader. Therefore, send method overwrites
	raftpb.Message's term with its HardState's term to avoid attaching its
	local term to 'MsgProp'. When 'MsgProp' is passed to the leader's 'Step'
	method, the leader first calls the 'appendEntry' method to append entries
	to its log, and then calls 'bcastAppend' method to send those entries to
	its peers. When passed to candidate, 'MsgProp' is dropped. When passed to
	follower, 'MsgProp' is stored in follower's mailbox(msgs) by the send
	method. It is stored with sender's ID and later forwarded to leader by
	rafthttp package.

	'MsgApp' contains log entries to replicate. A leader calls bcastAppend,
	which calls sendAppend, which sends soon-to-be-replicated logs in 'MsgApp'
	type. When 'MsgApp' is passed to candidate's Step method, candidate reverts
	back to follower, because it indicates that there is a valid leader sending
	'MsgApp' messages. Candidate and follower respond to this message in
	'MsgAppResp' type.

	'MsgAppResp' is response to log replication request('MsgApp'). When
	'MsgApp' is passed to candidate or follower's Step method, it responds by
	calling 'handleAppendEntries' method, which sends 'MsgAppResp' to raft
	mailbox.

	'MsgVote' requests votes for election. When a node is a follower or
	candidate and 'MsgHup' is passed to its Step method, then the node calls
	'campaign' method to campaign itself to become a leader. Once 'campaign'
	method is called, the node becomes candidate and sends 'MsgVote' to peers
	in cluster to request votes. When passed to leader or candidate's Step
	method and the message's Term is lower than leader's or candidate's,
	'MsgVote' will be rejected ('MsgVoteResp' is returned with Reject true).
	If leader or candidate receives 'MsgVote' with higher term, it will revert
	back to follower. When 'MsgVote' is passed to follower, it votes for the
	sender only when sender's last term is greater than MsgVote's term or
	sender's last term is equal to MsgVote's term but sender's last committed
	index is greater than or equal to follower's.

	'MsgVoteResp' contains responses from voting request. When 'MsgVoteResp' is
	passed to candidate, the candidate calculates how many votes it has won. If
	it's more than majority (quorum), it becomes leader and calls 'bcastAppend'.
	If candidate receives majority of votes of denials, it reverts back to
	follower.

	'MsgPreVote' and 'MsgPreVoteResp' are used in an optional two-phase election
	protocol. When Config.PreVote is true, a pre-election is carried out first
	(using the same rules as a regular election), and no node increases its term
	number unless the pre-election indicates that the campaigning node would win.
	This minimizes disruption when a partitioned node rejoins the cluster.

	'MsgSnap' requests to install a snapshot message. When a node has just
	become a leader or the leader receives 'MsgProp' message, it calls
	'bcastAppend' method, which then calls 'sendAppend' method to each
	follower. In 'sendAppend', if a leader fails to get term or entries,
	the leader requests snapshot by sending 'MsgSnap' type message.

	'MsgSnapStatus' tells the result of snapshot install message. When a
	follower rejected 'MsgSnap', it indicates the snapshot request with
	'MsgSnap' had failed from network issues which causes the network layer
	to fail to send out snapshots to its followers. Then leader considers
	follower's progress as probe. When 'MsgSnap' were not rejected, it
	indicates that the snapshot succeeded and the leader sets follower's
	progress to probe and resumes its log replication.

	'MsgHeartbeat' sends heartbeat from leader. When 'MsgHeartbeat' is passed
	to candidate and message's term is higher than candidate's, the candidate
	reverts back to follower and updates its committed index from the one in
	this heartbeat. And it sends the message to its mailbox. When
	'MsgHeartbeat' is passed to follower's Step method and message's term is
	higher than follower's, the follower updates its leaderID with the ID
	from the message.

	'MsgHeartbeatResp' is a response to 'MsgHeartbeat'. When 'MsgHeartbeatResp'
	is passed to leader's Step method, the leader knows which follower
	responded. And only when the leader's last committed index is greater than
	follower's Match index, the leader runs 'sendAppend` method.

	'MsgUnreachable' tells that request(message) wasn't delivered. When
	'MsgUnreachable' is passed to leader's Step method, the leader discovers
	that the follower that sent this 'MsgUnreachable' is not reachable, often
	indicating 'MsgApp' is lost. When follower's progress state is replicate,
	the leader sets it back to probe.

*/
package raft
