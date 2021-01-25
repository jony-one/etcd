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

package main

import (
	"flag"
	"strings"

	"go.etcd.io/etcd/v3/raft/raftpb"
)
/**
  主要的通信方式
		Client
          ↓
   GET/PUT/POST/DELETE
          ↓
      Server Listen →→ 1 →→ KV Store
								↓
								2
                                ↓
							Raft Node →→ 4 →→ Wal、Broadcast、Snapshot
								↓
								3
							    ↓
							   node
     → 客户端通过 HTTP 请求到 Server
	 → 当收到存储请求后首先会调用 KVStore 向我们通信管道 proposeC 发送数据
	 → raftNode模块从管道proposeC中监听到有数据事件到来后会将该数据继续往下层模块node传递
	 → 当raftNode收到从nkvode模块传递上来的准备就绪信息后就开始进行余下的操作，如wal日志写入-->更新raft示例的内存状态-->发送给集群其它peer-->更新自身状态机-->触发快照-->告知底层node我已处理完成，可以发送下一个消息.

	主要部分
	1.Leader 选举
	2.日志同步
	3.状态备份
 */
func main() {
	cluster := flag.String("cluster", "http://127.0.0.1:9021", "comma separated cluster peers")
	id := flag.Int("id", 1, "node ID")
	kvport := flag.Int("port", 9121, "key-value server port")
	join := flag.Bool("join", false, "join an existing cluster")
	flag.Parse()

	proposeC := make(chan string)  // 监听数据事件下发下层node 模块，应用与底层Raft核心库之间的通信channel，当用户向应用通过 http 发送更新请求时，应用会将此请求通过channel传递给底层的Raft库
	defer close(proposeC)
	confChangeC := make(chan raftpb.ConfChange)  // 配置文件
	defer close(confChangeC)

	// raft provides a commit stream for the proposals from the http api
	var kvs *kvstore
	getSnapshot := func() ([]byte, error) { return kvs.getSnapshot() } // 创建快照方法
	commitC, errorC, snapshotterReady := newRaftNode(*id, strings.Split(*cluster, ","), *join, getSnapshot, proposeC, confChangeC) //  对raftNode的初始化操作

	kvs = newKVStore(<-snapshotterReady, proposeC, commitC, errorC) //  对KVStore的初始化操作

	// the key-value http handler will propose updates to raft
	serveHttpKVAPI(kvs, *kvport, confChangeC, errorC) // 启动http监听
}
