package manager

import (
	"container/list"
	"golang.org/x/net/context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/wrapper"
	"github.com/cubefs/cubefs/util"
	"github.com/cubefs/cubefs/util/log"
)

const (
	runNow               = 1
	runLater             = 2
	gridHitLimitCnt      = 1
	girdCntOneSecond     = 3
	gridWindowTimeScope  = 10
	qosExpireTime        = 20
	qosReportMinGap      = 500
	defaultMagnifyFactor = 100
)

type UploadFlowInfoFunc func(clientInfo wrapper.SimpleClientInfo) error

type GridElement struct {
	time     time.Time
	used     uint64
	limit    uint64
	buffer   uint64
	hitLimit bool
	ID       uint64
	sync.RWMutex
}

type AllocElement struct {
	used    uint32
	magnify uint32
	future  *util.Future
}

type LimitFactor struct {
	factorType     uint32
	gridList       *list.List
	waitList       *list.List
	need           uint64
	gidHitLimitCnt uint8
	mgr            *LimitManager
	gridId         uint64
	magnify        uint32
	lock           sync.RWMutex
}

func (factor *LimitFactor) getNeedByMagnify(allocCnt uint32, magnify uint32) uint64 {
	if magnify == 0 {
		return 0
	}
	if allocCnt > 1000 {
		log.LogInfof("action[getNeedByMagnify] allocCnt %v", allocCnt)
		magnify = defaultMagnifyFactor
	}

	need := uint64(allocCnt * magnify)
	if factor.factorType == proto.FlowWriteType || factor.factorType == proto.FlowReadType {
		if need > util.GB/8 {
			need = util.GB / 8
		}
	}
	return need
}

func (factor *LimitFactor) alloc(allocCnt uint32) (ret uint8,future *util.Future) {
	//log.LogInfof("action[alloc] type [%v] alloc [%v], tmp factor waitlist [%v] limtcnt [%v] need [%v] len [%v]", proto.QosTypeString(factor.factorType),
	//	allocCnt, factor.waitList.Len(), factor.gidHitLimitCnt, factor.need, factor.gridList.Len())

	if !factor.mgr.enable {
		// used not accurate also fine, the purpose is get master's info
		// without lock can better performance just the used value large than 0
		gridEnd := factor.gridList.Back()
		if gridEnd != nil {
			grid := gridEnd.Value.(*GridElement)
			grid.used = grid.used+uint64(allocCnt)
		}
		return runNow, nil
	}

	type activeSt struct {
		activeUpdate bool
		needWait       bool
	}
	activeState := &activeSt{}
	defer func(active *activeSt) {
		if !active.needWait {
			factor.lock.RUnlock()
		} else if !active.activeUpdate {
			factor.lock.Unlock()
		}

	}(activeState)

	factor.lock.RLock()
	grid := factor.gridList.Back().Value.(*GridElement)

	if factor.mgr.enable && (factor.waitList.Len() > 0 || grid.used+uint64(allocCnt) > grid.limit+grid.buffer) {
		factor.lock.RUnlock()
		factor.lock.Lock()
		activeState.needWait = true
		future = util.NewFuture()

		factor.waitList.PushBack(&AllocElement{
			used:    allocCnt,
			future:  future,
			magnify: factor.magnify,
		})

		factor.need += factor.getNeedByMagnify(allocCnt, factor.magnify)

		if grid.hitLimit == false {
			factor.gidHitLimitCnt++
			// 1s have several gird, gidHitLimitCnt is the count that gird count hit limit in latest 1s,
			// if gidHitLimitCnt large than limit then request for enlarge factor limit
			// GetSimpleVolView will call back simpleClient function to get factor info and send to master
			if factor.gidHitLimitCnt >= factor.mgr.HitTriggerCnt {
				tmpTime := time.Now()
				if factor.mgr.lastReqTime.Add(time.Duration(factor.mgr.ReqPeriod) * time.Second).Before(tmpTime) {
					factor.mgr.lastReqTime = tmpTime
					log.LogInfof("CheckGrid factor [%v] unlock before active update simple vol view,gird id[%v] limit[%v] buffer [%v] used [%v]",
						proto.QosTypeString(factor.factorType), grid.ID, grid.limit, grid.buffer, grid.used)
					// unlock need call here,UpdateSimpleVolView will lock again
					factor.lock.Unlock()
					activeState.activeUpdate = true
					go factor.mgr.WrapperUpdate(factor.mgr.simpleClient)
				}
			}
		}
		grid.hitLimit = true
		return runLater, future
	}

	atomic.CompareAndSwapUint64(&grid.used, grid.used, grid.used+uint64(allocCnt))
	return runNow, future
}

func (factor *LimitFactor) SetLimit(limitVal uint64, bufferVal uint64) {
	log.LogInfof("acton[SetLimit] factor type [%v] limitVal [%v] bufferVal [%v]", proto.QosTypeString(factor.factorType), limitVal, bufferVal)
	var grid *GridElement
	factor.mgr.lastTimeOfSetLimit = time.Now()
	factor.lock.Lock()

	defer func() {
		factor.TryReleaseWaitList(grid)
		factor.lock.Unlock()
	}()

	if factor.gridList.Len() == 0 {
		grid = &GridElement{
			time:   time.Now(),
			limit:  limitVal / girdCntOneSecond,
			buffer: bufferVal / girdCntOneSecond,
			ID:     factor.gridId,
		}
		factor.gridId++
		factor.gridList.PushBack(grid)
	} else {
		grid = factor.gridList.Back().Value.(*GridElement)
		grid.buffer = bufferVal / girdCntOneSecond
		grid.limit = limitVal / girdCntOneSecond
	}
}

// clean wait list if limit be enlarged by master
// no lock need for parallel,caller own the lock and will release it
func (factor *LimitFactor) TryReleaseWaitList(newGrid *GridElement) {

	for factor.waitList.Len() > 0 {
		value := factor.waitList.Front()
		ele := value.Value.(*AllocElement)
		if newGrid.used+uint64(ele.used) > newGrid.limit+newGrid.buffer {
			log.LogWarnf("action[TryReleaseWaitList] type [%v] new gird be used up.alloc in waitlist left cnt [%v],"+
				"grid be allocated [%v] grid limit [%v] and buffer[%v], gird id:[%v]", proto.QosTypeString(factor.factorType),
				factor.waitList.Len(), newGrid.used, newGrid.limit, newGrid.buffer, newGrid.ID)
			break
		}
		newGrid.used += uint64(ele.used)
		ele.future.Respond(true, nil)

		factor.need -= factor.getNeedByMagnify(ele.used, ele.magnify)
		value = value.Next()
		factor.waitList.Remove(factor.waitList.Front())
	}
}

func (factor *LimitFactor) CheckGrid() {
	defer func() {
		factor.lock.Unlock()
	}()
	factor.lock.Lock()

	grid := factor.gridList.Back().Value.(*GridElement)
	newGrid := &GridElement{
		time:   time.Now(),
		limit:  grid.limit,
		used:   0,
		buffer: grid.buffer,
		ID:     factor.gridId,
	}
	factor.gridId++

	if factor.mgr.enable == true && factor.mgr.lastTimeOfSetLimit.Add(time.Second*qosExpireTime).Before(newGrid.time) {
		log.LogWarnf("action[CheckGrid]. qos disable in case of recv no command from master in long time")
	}
	log.LogInfof("action[CheckGrid] factor type:[%v] gridlistLen:[%v] waitlistLen:[%v] need:[%v] hitlimitcnt:[%v] "+
		"add new grid info used:[%v] limit:[%v] buffer:[%v] time:[%v]",
		proto.QosTypeString(factor.factorType), factor.gridList.Len(), factor.waitList.Len(), factor.need, factor.gidHitLimitCnt,
		newGrid.used, newGrid.limit, newGrid.buffer, newGrid.time)

	factor.gridList.PushBack(newGrid)
	for factor.gridList.Len() > gridWindowTimeScope*girdCntOneSecond {
		firstGrid := factor.gridList.Front().Value.(*GridElement)
		if firstGrid.hitLimit {
			factor.gidHitLimitCnt--
			log.LogInfof("action[CheckGrid] factor [%v] after minus gidHitLimitCnt:[%v]",
				factor.factorType, factor.gidHitLimitCnt)
		}
		log.LogInfof("action[CheckGrid] type:[%v] remove oldest grid info buffer:[%v] limit:[%v] used[%v] from gridlist",
			factor.factorType, firstGrid.buffer, firstGrid.limit, firstGrid.used)
		factor.gridList.Remove(factor.gridList.Front())
	}
	factor.TryReleaseWaitList(newGrid)

}

func newLimitFactor(mgr *LimitManager, factorType uint32) *LimitFactor {
	limit := &LimitFactor{
		mgr:        mgr,
		factorType: factorType,
		waitList:   list.New(),
		gridList:   list.New(),
		magnify:    defaultMagnifyFactor,
	}

	limit.SetLimit(0, 0)
	return limit
}

type LimitManager struct {
	ID                 uint64
	limitMap           map[uint32]*LimitFactor
	enable             bool
	simpleClient       wrapper.SimpleClientInfo
	exitCh             chan struct{}
	WrapperUpdate      UploadFlowInfoFunc
	ReqPeriod          uint32
	HitTriggerCnt      uint8
	lastReqTime        time.Time
	lastTimeOfSetLimit time.Time
	isLastReqValid     bool
	once               sync.Once
}

func NewLimitManager(client wrapper.SimpleClientInfo) *LimitManager {
	mgr := &LimitManager{
		limitMap:      make(map[uint32]*LimitFactor, 0),
		enable:        false, // assign from master
		simpleClient:  client,
		HitTriggerCnt: gridHitLimitCnt,
		ReqPeriod:     1,
	}
	mgr.limitMap[proto.IopsReadType] = newLimitFactor(mgr, proto.IopsReadType)
	mgr.limitMap[proto.IopsWriteType] = newLimitFactor(mgr, proto.IopsWriteType)
	mgr.limitMap[proto.FlowWriteType] = newLimitFactor(mgr, proto.FlowWriteType)
	mgr.limitMap[proto.FlowReadType] = newLimitFactor(mgr, proto.FlowReadType)

	mgr.ScheduleCheckGrid()
	return mgr
}

func (limitManager *LimitManager) GetFlowInfo() (*proto.ClientReportLimitInfo, bool) {
	log.LogInfof("action[LimitManager.GetFlowInfo]")
	info := &proto.ClientReportLimitInfo{
		FactorMap: make(map[uint32]*proto.ClientLimitInfo, 0),
	}
	var validCliInfo bool
	for factorType, limitFactor := range limitManager.limitMap {
		limitFactor.lock.RLock()

		var used, limit, buffer uint64
		grid := limitFactor.gridList.Back()
		// calc and set in case of time may be shifted
		griCnt := 0
		if limitFactor.gridList.Len() > 0 {
			log.LogInfof("action[GetFlowInfo] start  grid %v", grid.Value.(*GridElement).ID)
		}

		for griCnt < limitFactor.gridList.Len() {
			used += grid.Value.(*GridElement).used
			limit += grid.Value.(*GridElement).limit
			buffer += grid.Value.(*GridElement).buffer
			griCnt++
			if grid.Prev() == nil || griCnt >= girdCntOneSecond {
				break
			}

			grid = grid.Prev()
		}
		if limitFactor.gridList.Len() > 0 {
			log.LogInfof("action[GetFlowInfo] end grid %v", grid.Value.(*GridElement).ID)
		}

		timeElapse := time.Since(grid.Value.(*GridElement).time).Milliseconds()
		if timeElapse < qosReportMinGap {
			log.LogWarnf("action[GetFlowInfo] timeElapse [%v] since last report", timeElapse)
			timeElapse = qosReportMinGap // time of interval get vol view from master todo:change to config time
		}

		reqUsed := uint64(float64(used) / (float64(timeElapse) / 1000))
		if (limitFactor.factorType == proto.FlowReadType || limitFactor.factorType == proto.FlowWriteType) &&
			limitFactor.need > 0 {
			if reqUsed < util.MB {
				limitFactor.need = 5 * reqUsed
			} else if reqUsed < 5*util.MB {
				limitFactor.need = 2 * reqUsed
			} else if reqUsed < 10*util.MB {
				limitFactor.need = uint64(1.5 * float64(reqUsed))
			} else if reqUsed < 50*util.MB {
				limitFactor.need = uint64(1.2 * float64(reqUsed))
			} else if reqUsed < 100*util.MB {
				limitFactor.need = uint64(1.1 * float64(reqUsed))
			} else if reqUsed < 300*util.MB {
				limitFactor.need = reqUsed
			} else{
				limitFactor.need = 300 * util.MB
			}
		}

		factor := &proto.ClientLimitInfo{
			Used:		reqUsed,
			Need:       limitFactor.need,
			UsedLimit:  limitFactor.gridList.Back().Value.(*GridElement).limit * girdCntOneSecond,
			UsedBuffer: limitFactor.gridList.Back().Value.(*GridElement).buffer * girdCntOneSecond,
		}
		
		//if factor.Allocated > 100 && factor.NeedAfterAlloc > factor.Allocated {
		//	factor.NeedAfterAlloc = factor.Allocated
		//}

		limitFactor.lock.RUnlock()

		info.FactorMap[factorType] = factor
		info.Host = wrapper.LocalIP
		info.Status = proto.QosStateNormal
		info.ID = limitManager.ID
		if factor.Used|factor.Need|factor.UsedLimit|factor.UsedBuffer > 0 {
			validCliInfo = true
		}
		log.LogInfof("action[GetFlowInfo] type [%v] report to master with simpleClient limit info [%v,%v,%v,%v],host [%v], status [%v] grid [%v, %v, %v]",
			proto.QosTypeString(limitFactor.factorType),
			factor.Used, factor.Need, factor.UsedBuffer, factor.UsedLimit,
			info.Host, info.Status,
			grid.Value.(*GridElement).ID,
			grid.Value.(*GridElement).limit,
			grid.Value.(*GridElement).buffer)
	}

	lastValid := limitManager.isLastReqValid
	limitManager.isLastReqValid = validCliInfo

	limitManager.once.Do(func() {
		validCliInfo = true
	})
	// client has no user request then don't report to master
	if !lastValid && !validCliInfo {
		return info, false
	}
	return info, true
}

func (limitManager *LimitManager) ScheduleCheckGrid() {
	go func() {
		ticker := time.NewTicker(1000 / girdCntOneSecond * time.Millisecond)
		defer func() {
			ticker.Stop()
		}()

		for {
			select {
			case <-limitManager.exitCh:
				return
			case <-ticker.C:
				for _, limitFactor := range limitManager.limitMap {
					limitFactor.CheckGrid()
				}
			}
		}
	}()
}

func (limitManager *LimitManager) SetClientLimit(limit *proto.LimitRsp2Client) {
	if limit == nil {
		log.LogInfof("action[SetClientLimit] limit info is nil")
		return
	}
	log.LogInfof("action[SetClientLimit] limit enable %v", limit.Enable)
	if limitManager.enable != limit.Enable {
		log.LogWarnf("action[SetClientLimit] enable [%v]", limit.Enable)
	}
	limitManager.enable = limit.Enable
	if limit.HitTriggerCnt > 0 {
		log.LogWarnf("action[SetClientLimit] update to HitTriggerCnt [%v] from [%v]", limitManager.HitTriggerCnt, limit.HitTriggerCnt)
		limitManager.HitTriggerCnt = limit.HitTriggerCnt
	}
	if limit.ReqPeriod > 0 {
		log.LogWarnf("action[SetClientLimit] update to ReqPeriod [%v] from [%v]", limitManager.ReqPeriod, limit.ReqPeriod)
		limitManager.ReqPeriod = limit.ReqPeriod
	}

	for factorType, clientLimitInfo := range limit.FactorMap {
		limitManager.limitMap[factorType].SetLimit(clientLimitInfo.UsedLimit, clientLimitInfo.UsedBuffer)
	}
	for factorType, magnify := range limit.Magnify {
		if magnify > 0 && magnify != limitManager.limitMap[factorType].magnify {
			log.LogInfof("action[SetClientLimit] type [%v] update magnify [%v] to [%v]",
				proto.QosTypeString(factorType), limitManager.limitMap[factorType].magnify, magnify)
			limitManager.limitMap[factorType].magnify = magnify
		}
	}
}

func (limitManager *LimitManager) ReadAlloc(ctx context.Context, size int) {
	limitManager.WaitN(ctx, limitManager.limitMap[proto.IopsReadType], 1)
	limitManager.WaitN(ctx, limitManager.limitMap[proto.FlowReadType], size)
}

func (limitManager *LimitManager) WriteAlloc(ctx context.Context, size int) {
	limitManager.WaitN(ctx, limitManager.limitMap[proto.IopsWriteType], 1)
	limitManager.WaitN(ctx, limitManager.limitMap[proto.FlowWriteType], size)
}

// WaitN blocks until alloc success
func (limitManager *LimitManager) WaitN(ctx context.Context, lim *LimitFactor, n int) (err error) {
	var fut *util.Future
	var ret uint8
	if ret, fut = lim.alloc(uint32(n)); ret == runNow {
		// log.LogInfof("action[WaitN] type [%v] return now waitlistlen [%v]", proto.QosTypeString(lim.factorType), lim.waitList.Len())
		return nil
	}

	respCh, errCh := fut.AsyncResponse()

	select {
	case <-ctx.Done():
		log.LogWarnf("action[WaitN] type [%v] ctx done return waitlistlen [%v]", proto.QosTypeString(lim.factorType), lim.waitList.Len())
		return ctx.Err()
	case err = <-errCh:
		log.LogWarnf("action[WaitN] type [%v] err return waitlistlen [%v]", proto.QosTypeString(lim.factorType), lim.waitList.Len())
		return
	case <-respCh:
		log.LogInfof("action[WaitN] type [%v] return waitlistlen [%v]", proto.QosTypeString(lim.factorType), lim.waitList.Len())
		return nil
		//default:
	}
}

func (limitManager *LimitManager) UpdateFlowInfo(limit *proto.LimitRsp2Client) {
	log.LogInfof("action[LimitManager.UpdateFlowInfo]")
	limitManager.SetClientLimit(limit)
	return
}

func (limitManager *LimitManager) SetClientID(id uint64) (err error) {
	limitManager.ID = id
	return
}