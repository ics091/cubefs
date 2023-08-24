package cfs

import (
	"fmt"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/master"
	"github.com/cubefs/cubefs/util/checktool/ump"
	"github.com/cubefs/cubefs/util/config"
	"github.com/cubefs/cubefs/util/exporter"
	"github.com/cubefs/cubefs/util/log"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

const (
	cfgFixBadPartition = "fixBadPartition"
	cfgUmpAPiToken     = "umpToken"
	endPoint           = "spark_master_warning"
	umpOpenAPiDomain   = "open.ump.jd.com"
	alarmRecordsMethod = "/alarm/records"
	cfsDomain          = "sparkchubaofs.jd.local"
)

func (s *ChubaoFSMonitor) scheduleToFixBadDataPartition(cfg *config.Config) {
	if !cfg.GetBool(cfgFixBadPartition) {
		return
	}
	if cfg.GetString(cfgUmpAPiToken) == "" {
		log.LogErrorf("ump token not found in config")
		return
	}
	log.LogInfof("scheduleToFixBadDataPartition started")
	s.umpClient = ump.NewUmpClient(cfg.GetString(cfgUmpAPiToken), umpOpenAPiDomain)
	var fixTick = time.NewTicker(time.Second)
	defer fixTick.Stop()
	for {
		select {
		case <-fixTick.C:
			s.doFixBadDataPartition()
			fixTick.Reset(time.Minute)
		}
	}
}

func (s *ChubaoFSMonitor) doFixBadDataPartition() {
	var err error
	var alarmRecords *ump.AlarmRecordResponse
	defer func() {
		if r := recover(); r != nil {
			log.LogErrorf("action[doFixBadDataPartition] recover from panic:%v", r)
		}
		if err != nil {
			log.LogErrorf("action[doFixBadDataPartition] err:%v", err)
		}
	}()
	idsMap := make(map[uint64]bool, 0)
	alarmRecords, err = s.umpClient.GetAlarmRecords(alarmRecordsMethod, "chubaofs-node", "jdos", endPoint, time.Now().UnixMilli()-60*10*1000, time.Now().UnixMilli())
	if err != nil {
		return
	}
	for _, r := range alarmRecords.Records {
		if strings.Contains(r.Content, "FIX DataPartition replicaNum") {
			tmp := strings.Split(r.Content, "partitionID:")[1]
			pidStr := strings.Split(tmp, " ")[0]
			pid, e := strconv.ParseUint(pidStr, 10, 64)
			if e != nil {
				log.LogErrorf("parse partition id failed:%v", e)
				continue
			}
			idsMap[pid] = true
		}
	}
	if len(idsMap) == 0 {
		return
	}
	client := master.NewMasterClient([]string{cfsDomain}, false)
	topologyView, err := client.AdminAPI().GetTopology()
	if err != nil {
		return
	}
	log.LogWarnf("action[doFixBadDataPartition] domain[sparkchubaofs.jd.local] found %v bad partitions, start fix", len(idsMap))
	for partition := range idsMap {
		var (
			needFix    bool
			extraHost  string
			remainHost string
		)
		if needFix, extraHost, remainHost, err = isNeedFix(client, partition); err != nil {
			log.LogErrorf("action[doFixBadDataPartition] err:%v", err)
			continue
		}
		if !needFix {
			continue
		}
		var dn *proto.DataNodeInfo
		dn, err = client.NodeAPI().GetDataNode(extraHost)
		if err != nil {
			log.LogErrorf("action[doFixBadDataPartition] err:%v", err)
			continue
		}
		allNodeViews := make([]proto.NodeView, 0)
		for _, zone := range topologyView.Zones {
			if zone.Name != dn.ZoneName {
				continue
			}
			for _, ns := range zone.NodeSet {
				allNodeViews = append(allNodeViews, ns.DataNodes...)
			}
		}
		log.LogInfof("action[doFixBadDataPartition] try to add learner to fix one replica partition:%v", partition)
		retry := 20
		for i := 0; i < retry; i++ {
			rand.Seed(time.Now().UnixNano())
			index := rand.Intn(len(allNodeViews) - 1)
			destNode := allNodeViews[index]
			if destNode.Addr == extraHost || destNode.Addr == remainHost {
				continue
			}
			var destNodeView *proto.DataNodeInfo
			destNodeView, err = client.NodeAPI().GetDataNode(destNode.Addr)
			if err != nil {
				log.LogErrorf("action[doFixBadDataPartition] err:%v", err)
				continue
			}
			if destNodeView.UsageRatio > 0.8 {
				continue
			}
			if !destNodeView.IsActive {
				continue
			}
			err = client.AdminAPI().AddDataLearner(partition, destNode.Addr, true, 90)
			if err != nil {
				log.LogErrorf("action[doFixBadDataPartition] partition:%v, err:%v", partition, err)
				break
			}
			exporter.WarningBySpecialUMPKey(UMPCFSSparkFixPartitionKey, fmt.Sprintf("Domain[%v] fix one replica partition:%v success, add learner:%v", cfsDomain, partition, destNode.Addr))
			break
		}
	}
	return
}

func isNeedFix(client *master.MasterClient, partition uint64) (fix bool, extraHost string, remainHost string, err error) {
	var dp *proto.DataPartitionInfo
	dp, err = client.AdminAPI().GetDataPartition("", partition)
	if err != nil {
		return
	}
	if dp.ReplicaNum != 2 {
		return
	}
	if len(dp.Hosts) != 1 {
		return
	}
	leader := false
	for _, r := range dp.Replicas {
		if r.IsLeader {
			leader = true
		}
	}
	if !leader {
		err = fmt.Errorf("partition:%v no leader", partition)
		return
	}
	// len(hosts)==1, retry 20s later
	time.Sleep(time.Second * 20)
	dp, err = client.AdminAPI().GetDataPartition("", partition)
	if err != nil {
		return
	}
	if len(dp.Hosts) != 1 {
		return
	}
	for _, replica := range dp.Replicas {
		if replica.Addr == dp.Hosts[0] {
			continue
		}
		extraHost = replica.Addr
		break
	}
	remainHost = dp.Hosts[0]
	fix = true
	return
}