package shardkv

import "labrpc"
import "raft"
import "sync"
import "labgob"
import "log"
import "time"
import "bytes"
import "shardmaster"
import "strconv"

type Op struct {
	Ch  chan (interface{})
	Req interface{}
}

var shardKvOnce sync.Once

type ShardKV struct {
	mu           sync.Mutex
	me           int
	rf           *raft.Raft
	applyCh      chan raft.ApplyMsg
	make_end     func(string) *labrpc.ClientEnd
	gid          int
	masters      []*labrpc.ClientEnd
	maxraftstate int // snapshot if log grows this big

	kvs            [shardmaster.NShards] map[string]string
	msgIDs         map[int64] int64
	killChan       chan (bool)
	killed         bool
	persister      *raft.Persister
	logApplyIndex  int
	
	config         shardmaster.Config
	nextConfig     shardmaster.Config
	deleteShardsNum      int
	mck            *shardmaster.Clerk
	timer          *time.Timer         
	EnableDebugLog bool

}

func (kv *ShardKV) println(args ...interface{}) {
	if kv.EnableDebugLog {
		log.Println(args...)
	}
}
//判定重复请求
func (kv *ShardKV) isRepeated(client int64,msgId int64) bool {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	rst := false
	index,ok := kv.msgIDs[client]
	if ok {
		rst = index >= msgId
	}
	kv.msgIDs[client] = msgId
	return rst
}

//判定cofig更新完成
func (kv *ShardKV) cofigCompleted(Num int) bool {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return  kv.config.Num >= Num
}
//获取新增shards
func (kv *ShardKV) getNewShards() (map[int][]int,bool) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	shards := make(map[int][]int)
	if kv.nextConfig.Num > kv.config.Num {
		if kv.nextConfig.Num >1 {
			oldShards := GetGroupShards(&(kv.config.Shards),kv.gid) //旧该组分片
			newShards := GetGroupShards(&(kv.nextConfig.Shards),kv.gid) //新该组分片
			for key,_ := range newShards {  //获取新增组
				_,ok := oldShards[key]
				if !ok {
					group := kv.config.Shards[key]
					value,ok := shards[group]
					if !ok {
						value = make([]int,0)
					}
					value = append(value,key)
					shards[group] = value
				}
			}
		}
		return shards,true
	}
	return shards,false
}

func (kv *ShardKV) setConfig(config *shardmaster.Config) bool  {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if config.Num > kv.config.Num {
		kv.config = *config
		return true
	}
	return false
}
//raft更新shard
func (kv *ShardKV) startShard(shards *RespShareds) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if kv.config.Num < kv.nextConfig.Num {
		op := Op {
			Req : *shards, //请求数据
			Ch : make(chan(interface{})), //日志提交chan
		}
		index,term,isLeader := kv.rf.Start(op) //写入Raft
		if isLeader {
			kv.println(kv.gid,kv.me,"start shard ", kv.nextConfig.Num,"term",term,"index",index)
		}
	}
}
//raft更新config
func (kv *ShardKV) startConfig(config *shardmaster.Config) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if config.Num > kv.nextConfig.Num && kv.nextConfig.Num == kv.config.Num {
		op := Op {
			Req : *config, //请求数据
			Ch : make(chan(interface{})), //日志提交chan
		}
		kv.rf.Start(op) //写入Raft
	}
}
//判定重复请求
func (kv *ShardKV) isTrueGroup(shard int) bool{
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if kv.config.Num == 0 {
		return false
	}
	return kv.config.Shards[shard] == kv.gid && kv.nextConfig.Num == kv.config.Num
}

//raft操作
func (kv *ShardKV) opt(req interface{}) (bool,interface{}) {
	op := Op {
		Req : req, //请求数据
		Ch : make(chan(interface{}),1), //日志提交chan
	}
	_, _, isLeader := kv.rf.Start(op) //写入Raft
	if !isLeader {  //判定是否是leader
		return false,nil  
	}
	select {
		case resp := <-op.Ch:
			return true,resp
		case <-time.After(time.Millisecond * 1000): //超时
	}
	return false,nil
}

func (kv *ShardKV) Get(req *GetArgs, reply *GetReply) {
	ok,value := kv.opt(*req)
	reply.WrongLeader = !ok
	if ok {
		*reply = value.(GetReply)
	}
}

func (kv *ShardKV) PutAppend(req *PutAppendArgs, reply *PutAppendReply) {
	ok,value := kv.opt(*req)
	reply.WrongLeader = !ok
	if ok {
		reply.Err = value.(Err)
	}
}

func (kv *ShardKV) GetShard(req *ReqShared, reply *RespShared) {
	ok,value := kv.opt(*req)
	reply.Successed = false 
	if ok {
		*reply = value.(RespShared)
	}
}

func (kv *ShardKV) Kill() {
	kv.killed = true
	kv.rf.Kill()
	kv.killChan <- true
}

//判定是否写入快照
func  (kv *ShardKV) ifSaveSnapshot(save bool) {
	if kv.maxraftstate != -1 && (kv.persister.RaftStateSize() >= kv.maxraftstate || save){
		writer := new(bytes.Buffer)
		encoder := labgob.NewEncoder(writer)
		encoder.Encode(kv.msgIDs)	
		encoder.Encode(kv.kvs)
		encoder.Encode(kv.config)
		encoder.Encode(kv.nextConfig)
		data := writer.Bytes()
		kv.rf.SaveSnapshot(kv.logApplyIndex,data)
		return 
	}
}
//更新快照
func  (kv *ShardKV) updateSnapshot(index int,data []byte) {
	if data == nil || len(data) < 1 || kv.maxraftstate == -1 {
		return
	}
	kv.logApplyIndex = index
	reader := bytes.NewBuffer(data)
	decoder := labgob.NewDecoder(reader)
	for i:=0; i<len(kv.kvs); i++ {
		kv.kvs[i] = make(map[string]string)
	}
	if decoder.Decode(&kv.msgIDs) != nil ||
		decoder.Decode(&kv.kvs) != nil   ||
		decoder.Decode(&kv.config) != nil ||
		decoder.Decode(&kv.nextConfig) != nil {
		kv.println("Error in unmarshal raft state")
	}
}
//写操作
func (kv *ShardKV) putAppend(req *PutAppendArgs) Err {
	if !kv.isTrueGroup(req.Shard) {
		return ErrWrongGroup
	}
	if !kv.isRepeated(req.Me,req.MsgId) { //去重复
		if req.Op == "Put" {
			kv.kvs[req.Shard][req.Key] = req.Value
		} else if req.Op == "Append" {
			value, ok := kv.kvs[req.Shard][req.Key]
			if !ok {
				value = ""
			}
			value += req.Value
			kv.kvs[req.Shard][req.Key] = value
		}
		kv.println(kv.gid,kv.me,"on",req.Op,req.Shard,"client",req.Me,"msgid:",req.MsgId,req.Key,":",req.Value,kv.kvs[req.Shard][req.Key])
	}
	return OK
}
//读操作
func (kv *ShardKV) get(req *GetArgs) (resp GetReply) {
	if !kv.isTrueGroup(req.Shard) {
		resp.Err = ErrWrongGroup
		return 
	}
	value, ok := kv.kvs[req.Shard][req.Key]
	resp.WrongLeader = false
	resp.Err = OK
	if !ok {
		value = ""
		resp.Err = ErrNoKey
	}
	resp.Value = value
	kv.println(kv.gid,kv.me,"on get",req.Shard,req.Key,":",value)
	return 
}

//删除已发送分片
func (kv *ShardKV) onDeleteShards(req *ReqDeleteShared) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if kv.config.Num == req.ConfigNum {
		info := "" 
		for i:=0;i<len(req.Shards);i++ {
			shard := req.Shards[i]
			kv.kvs[shard] = make(map[string]string)
			info += strconv.Itoa(shard)
			info += " "
		}
		kv.println(kv.gid,kv.me,"on delete",kv.config.Num," config shards",info)
	}
}

//删除已发送分片
func (kv *ShardKV) DeleteShards(req *ReqDeleteShared,resp *RespDeleteShared) {
	if kv.config.Num == req.ConfigNum && req.ConfigNum > kv.deleteShardsNum {
		kv.deleteShardsNum = req.ConfigNum
		kv.opt(*req)
	}
}

//请求删除已获得分片
func (kv *ShardKV) deleteGroupShards(config *shardmaster.Config,resp *RespShareds) {
	for group,value := range resp.Shards {
		req := ReqDeleteShared {
			ConfigNum : resp.Config.Num ,
		}
		for shard,_ := range value.Data {
			req.Shards = append(req.Shards,shard)
		}
		servers, ok := config.Groups[group]  //获取目标组服务
		if !ok  {
            continue 
        }
        resp :=RespDeleteShared{}
		for i := 0; i < len(servers); i++ {
			server := kv.make_end(servers[i])
			server.Call("ShardKV.DeleteShards", &req, &resp)
		}
	}
}
//设置分片数据
func (kv *ShardKV) onSetShard(resp *RespShareds) {
	if kv.cofigCompleted(resp.Config.Num) { //数据已更新
		kv.println(kv.gid,kv.me,"not set config :",resp.Config.Num)
		return 
	}
	//更新数据
	for _,value := range resp.Shards {
		for shard,kvs := range value.Data {
			for key,data := range kvs {
				kv.kvs[shard][key] = data
			}
		}
	}
	//更新msgid
	kv.mu.Lock()
	for _,shard := range resp.Shards {
		for key,value := range shard.MsgIDs {
			id,ok := kv.msgIDs[key]
			if !ok || id < value {
				kv.msgIDs[key] = value
			}
		}
	}
	preConfig := kv.config
	kv.nextConfig = resp.Config
	kv.config = resp.Config
	kv.println(kv.gid,kv.me,"on set config :",kv.config.Num)
	kv.mu.Unlock()
	go kv.deleteGroupShards(&preConfig,resp)
}

func (kv *ShardKV) onGetShard(req *ReqShared) (resp RespShared) {
	if req.Config.Num > kv.config.Num { //自己未获取最新数据
		resp.Successed = false 
		kv.timer.Reset(0) //获取最新数据
		return 
	}
	resp.Successed = true
	//复制已处理消息
	resp.MsgIDs = make(map[int64] int64)
	for key,value := range kv.msgIDs {
		resp.MsgIDs[key] = value
	}
	//复制分片数据
	resp.Data = make(map[int]map[string]string)
	for i:=0;i<len(req.Shards);i++ {
		shard := req.Shards[i]
		data := kv.kvs[shard]
		shardDatas := make(map[string]string)
		for key,value := range data{
			shardDatas[key] = value	
		}		
		resp.Data[shard] = shardDatas
	}

	return 
}

func (kv *ShardKV) onConfig(config *shardmaster.Config) {
	if config.Num > kv.nextConfig.Num {
		kv.mu.Lock()
		defer kv.mu.Unlock()
		kv.nextConfig = *config
	}
}
//获取新配置
func (kv *ShardKV) updateConfig()  {
	if _, isLeader := kv.rf.GetState(); isLeader {
		if kv.nextConfig.Num == kv.config.Num {
			config := kv.mck.Query(kv.nextConfig.Num+1)
			kv.startConfig(&config)
		}
	}
}

//apply 状态机
func (kv *ShardKV) onApply(applyMsg raft.ApplyMsg) {
	if !applyMsg.CommandValid {  //非状态机apply消息
		if command, ok := applyMsg.Command.(raft.LogSnapshot); ok { //更新快照消息
			kv.updateSnapshot(command.Index,command.Datas)
		}  else {
			kv.ifSaveSnapshot(false)
		}
		return
	}
	//更新日志索引，用于创建最新快照
	kv.logApplyIndex = applyMsg.CommandIndex
	opt := applyMsg.Command.(Op)
	var resp interface{}
	if command, ok := opt.Req.(PutAppendArgs); ok { //Put && append操作
		resp = kv.putAppend(&command)
	} else if command, ok := opt.Req.(GetArgs); ok { //Get操作
		resp = kv.get(&command)
	} else if command, ok := opt.Req.(shardmaster.Config);ok {  //更新config
		kv.onConfig(&command)
	} else if command, ok := opt.Req.(ReqShared);ok {  //其他组获取该组分片数据
		resp = kv.onGetShard(&command)
	} else if command, ok := opt.Req.(RespShareds);ok {  //获取其他组分片	
		kv.onSetShard(&command)
	} else if command, ok := opt.Req.(ReqDeleteShared); ok {
		kv.onDeleteShards(&command)
		kv.ifSaveSnapshot(true)  //强制刷新快照数据
	}
	select {
		case opt.Ch <- resp :
		default:
	}
}
//轮询
func (kv *ShardKV) mainLoop() {
	defer kv.println(kv.gid,kv.me,"Exit mainLoop")
	duration := time.Duration(time.Millisecond * 10)
	for !kv.killed {
		select {
		case <-kv.killChan:
			return
		case msg := <-kv.applyCh:
			if cap(kv.applyCh) - len(kv.applyCh) < 5 {
				log.Println(kv.gid,kv.me,"warn : maybe dead lock...")
			}
			kv.onApply(msg)
		case <-kv.timer.C :
			kv.updateConfig()
			kv.timer.Reset(duration)
		}
	}
	
}
func (kv *ShardKV) isLeader() bool  {
	_,rst := kv.rf.GetState()
	return rst
}

//请求其他组数据
func (kv *ShardKV) getShardLoop() {
	defer kv.println(kv.gid,kv.me,"Exit ShardLoop")
	for !kv.killed {
		if !kv.isLeader() {
			time.Sleep(time.Millisecond*10)
			continue
		}
		shards,isUpdated := kv.getNewShards() //获取新增分片
		config := kv.nextConfig
		if !isUpdated {
			time.Sleep(time.Millisecond*10)
			continue
		}
		kv.println(kv.gid,kv.me,"update config",kv.nextConfig.Num,":",shardmaster.GetShardsInfoString(&(kv.nextConfig.Shards)))
		if len(shards) >0 {
			kv.println(kv.gid,kv.me,"new shards",kv.nextConfig.Num,":",GetGroupShardsString(shards))
		}
		waitCh := make(chan bool,len(shards))
		rst := make(map[int]RespShared)
		var mutex sync.Mutex
		for key,value := range shards {  //遍历所有分片，请求数据
			go func(group int,shards []int) {
				defer func() {waitCh<-true}()
				complet := false
				var reply RespShared
				for !complet && !(kv.cofigCompleted(config.Num)) && !kv.killed {
					servers, ok := kv.config.Groups[group]  //获取目标组服务
					if !ok  {
                        kv.println(kv.gid,kv.me,"Error : can not get group",group,"shard data")
                        time.Sleep(time.Millisecond * 500)
                        continue 
                    }
                    req := ReqShared  {
                        Config : config,
                        Shards : shards,
                    } 
					for i := 0; i < len(servers); i++ {
						server := kv.make_end(servers[i])
						ok := server.Call("ShardKV.GetShard", &req, &reply)
						if ok && reply.Successed {
							complet = true
							break
						}
						if kv.cofigCompleted(config.Num) || kv.killed{
							break
						}
						time.Sleep(time.Millisecond*10)
					}
				}
				//存储该分片数据
				if !kv.cofigCompleted(config.Num) && !kv.killed{
					mutex.Lock()
					rst[group] = reply
					mutex.Unlock()
				}
			}(key,value)
		}
		isTimeout  := false
		for i:=0;i<len(shards) && !kv.killed && !isTimeout; { 
			if kv.cofigCompleted(config.Num) {
				break
			}

			select  {
				case <- waitCh :
					i++
				case <-time.After(time.Millisecond * 5000): //超时
					isTimeout = true	
			}	
		}
		if isTimeout || kv.cofigCompleted(config.Num)  || !kv.isLeader() {
			time.Sleep(time.Millisecond*10)
			continue
		}
		//获取的状态写入RAFT，直到成功
		for !(kv.cofigCompleted(config.Num)) && !kv.killed && kv.isLeader() {
			respShards := RespShareds{
				Config : config ,
				Shards : rst,
			} 
			kv.startShard(&respShards)
			time.Sleep(time.Millisecond*1000)
		}
	}
}

func StartServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxraftstate int, gid int, masters []*labrpc.ClientEnd, make_end func(string) *labrpc.ClientEnd) *ShardKV {
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Op{})

	kv := new(ShardKV)
	kv.me = me
	kv.maxraftstate = maxraftstate
	kv.make_end = make_end
	kv.gid = gid
	kv.masters = masters
	
	for i:=0;i<shardmaster.NShards;i++ {
		kv.kvs[i] = make(map[string]string)
	}
	kv.msgIDs = make(map[int64]int64)
	kv.killChan = make(chan (bool),2)
	kv.persister = persister
	kv.logApplyIndex = 0
	shardKvOnce.Do(func() {
		labgob.Register(Op{})
		labgob.Register(PutAppendArgs{})
		labgob.Register(GetArgs{})
		labgob.Register(ReqShared{})
		labgob.Register(RespShared{})
		labgob.Register(RespShareds{})
		labgob.Register(ReqDeleteShared{})
		labgob.Register(RespDeleteShared{})
		labgob.Register(shardmaster.Config{})
	})
	kv.applyCh = make(chan raft.ApplyMsg, 1000)
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)
	kv.mck = shardmaster.MakeClerk(kv.masters)
	kv.rf.EnableDebugLog = false
	kv.EnableDebugLog = false
	kv.config.Num = 0
	kv.nextConfig.Num = 0
	kv.deleteShardsNum = 0
	kv.killed = false
	kv.timer = time.NewTimer(time.Duration(time.Millisecond * 100))
	go kv.mainLoop()
	go kv.getShardLoop()
	return kv
}
