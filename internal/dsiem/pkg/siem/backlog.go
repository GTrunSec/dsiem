package siem

import (
	"dsiem/internal/dsiem/pkg/asset"
	"dsiem/internal/dsiem/pkg/event"
	log "dsiem/internal/shared/pkg/logger"
	"dsiem/internal/shared/pkg/str"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/viper"

	"github.com/elastic/apm-agent-go"
)

var bLogFile string

type backLog struct {
	sync.RWMutex
	ID           string    `json:"backlog_id"`
	StatusTime   int64     `json:"status_time"`
	Risk         int       `json:"risk"`
	CurrentStage int       `json:"current_stage"`
	HighestStage int       `json:"highest_stage"`
	Directive    directive `json:"directive"`
	SrcIPs       []string  `json:"src_ips"`
	DstIPs       []string  `json:"dst_ips"`
	LastEvent    event.NormalizedEvent
	chData       chan *event.NormalizedEvent
	chDone       chan struct{}
	chFound      chan bool
	deleted      bool      // flag for deletion process
	bLogs        *backlogs // pointer to parent, for locking delete operation?
}

type siemAlarmEvents struct {
	ID    string `json:"alarm_id"`
	Stage int    `json:"stage"`
	Event string `json:"event_id"`
}

func (b *backLog) worker(initialEvent *event.NormalizedEvent) {

	debug := viper.GetBool("debug")
	// first, process the initial event
	b.processMatchedEvent(initialEvent, 0)
	// b.info("after processmatchedevent", initialEvent.ConnID)

	go func() {
		for {
			evt, ok := <-b.chData
			if !ok {
				b.debug("worker chData closed, exiting", 0)
				return
			}
			// b.info("backlog incoming event", evt.ConnID)
			b.RLock()
			cs := b.CurrentStage
			if cs <= 1 {
				// b.info("backlog cs <= 1", evt.ConnID)
				b.RUnlock()
				continue
			}
			// should check for currentStage rule match with event
			// heuristic, we know stage starts at 1 but rules start at 0
			idx := cs - 1
			currRule := b.Directive.Rules[idx]
			if !doesEventMatchRule(evt, &currRule, evt.ConnID) {
				// b.info("backlog doeseventmatch false", evt.ConnID)
				b.chFound <- false
				b.RUnlock()
				continue
			}
			b.chFound <- true // answer quickly
			b.RUnlock()

			if !b.isTimeInOrder(evt) { // discard out of order event
				continue
			}

			// this should be under go routine, but chFound need sync access (for first match, backlog creation)
			if cs == 1 {
				b.processMatchedEvent(evt, idx)
			} else {
				b.processMatchedEvent(evt, idx) // use go routine later
			}
			// b.info("setting found to true", evt.ConnID)
		}
	}()

	go func() {
		// create own ticker here
		ticker := time.NewTicker(time.Second * 10)
		for {
			<-ticker.C
			b.debug("backlog tick started.", 0)
			select {
			case <-b.chDone:
				b.debug("backlog tick exiting, chDone.", 0)
				return
			default:
			}
			if debug {
				b.dumpCurrentRule(false)
			}
			if !b.isExpired() {
				continue
			}
			ticker.Stop() // prevent next signal, we're exiting the go routine
			b.info("backlog expired, deleting it", 0)
			// b.setStatus("timeout", 0, tx)
			b.delete()
		}
	}()
	b.debug("exiting worker, leaving routine behind", initialEvent.ConnID)

}

// no modification so use value receiver
func (b backLog) isTimeInOrder(e *event.NormalizedEvent) bool {
	// exit if in first stage
	cs := b.CurrentStage
	if cs == 1 {
		return true
	}
	prevStageTime := b.Directive.Rules[cs-1].EndTime
	t, err := time.Parse(time.RFC3339, e.Timestamp)
	if err != nil {
		b.warn("Cannot parse the event timestamp: "+err.Error(), e.ConnID)
		return false
	}
	currTime := t.Unix() + 5 // allow up to 5 seconds diff to compensate for concurrent write
	if prevStageTime > currTime {
		fmt.Println("Event discarded prevTime:", prevStageTime, " eventTime:", t.Unix())
		return false
	}
	return true
}

func (b backLog) dumpCurrentRule(listEvent bool) {
	fmt.Println("Directive ID:", b.Directive.ID, "backlog ID: ", b.ID)
	for i := range b.Directive.Rules {
		if listEvent {
			fmt.Println("Rule:", i, ":", b.Directive.Rules[i].Events)
		} else {
			fmt.Println("Rule:", i, "length:", len(b.Directive.Rules[i].Events))
		}
	}
}

func (b backLog) isExpired() bool {
	now := time.Now().Unix()
	cs := b.CurrentStage
	idx := cs - 1
	start := b.Directive.Rules[idx].StartTime
	timeout := b.Directive.Rules[idx].Timeout
	maxTime := start + timeout
	if maxTime >= now {
		return false
	}
	return true
}

func (b *backLog) processMatchedEvent(e *event.NormalizedEvent, idx int) {

	tx := elasticapm.DefaultTracer.StartTransaction("Directive Event Processing", "SIEM")
	tx.Context.SetCustom("event_id", e.EventID)
	b.RLock()
	tx.Context.SetCustom("backlog_id", b.ID)
	tx.Context.SetCustom("directive_id", b.Directive.ID)
	tx.Context.SetCustom("backlog_stage", b.CurrentStage)
	b.RUnlock()
	defer tx.End()
	defer elasticapm.DefaultTracer.Recover(tx)

	b.debug("Incoming event with idx: "+strconv.Itoa(idx), e.ConnID)
	// concurrent write may make events count overflow, so dont append current stage unless needed
	if !b.isStageReachMaxEvtCount(idx) {
		b.appendandWriteEvent(e, idx, tx)
		// exit early if the newly added event hasnt caused events_count == occurrence
		// for the current stage
		if !b.isStageReachMaxEvtCount(idx) {
			// deprecate this
			// b.ensureStatusAndStartTime(idx, e.ConnID, tx)
			return
		}
	}
	// b.info("setting status to finish", e.ConnID)
	// the new event has caused events_count == occurrence
	// b.setStatus("finished", e.ConnID, tx)
	b.setRuleEndTime(idx)
	b.updateAlarm(e.ConnID, tx)

	// if it causes the last stage to reach events_count == occurrence, delete it
	if b.isLastStage() {
		b.info("reached max stage and occurrence, deleting.", e.ConnID)
		b.delete()
		tx.Result = "Backlog removed (max reached)"
		return
	}

	// reach max occurrence, but not in last stage. Increase stage.
	if err := b.increaseStage(e); err != nil {
		return
	}
	b.updateAlarm(e.ConnID, tx)

	// b.setStatus("active", e.ConnID, tx)
	b.RLock()
	tx.Context.SetCustom("backlog_stage", b.CurrentStage)
	b.RUnlock()
	tx.Result = "Stage increased"

	// recalc risk, the new stage will have a different reliability
	riskChanged := b.calcRisk(e.ConnID)
	if riskChanged {
		// this LastEvent is used to get ports by alarm
		b.setLastEvent(e)
		b.updateAlarm(e.ConnID, tx)
	}
}

func (b backLog) info(msg string, connID uint64) {
	log.Info(log.M{Msg: msg, BId: b.ID, CId: connID})
}

func (b backLog) warn(msg string, connID uint64) {
	log.Warn(log.M{Msg: msg, BId: b.ID, CId: connID})
}

func (b backLog) debug(msg string, connID uint64) {
	log.Debug(log.M{Msg: msg, BId: b.ID, CId: connID})
}

func (b *backLog) setLastEvent(e *event.NormalizedEvent) {
	b.Lock()
	b.LastEvent = *e
	b.Unlock()
}

func (b *backLog) updateAlarm(connID uint64, tx *elasticapm.Transaction) {
	upsertAlarmFromBackLog(*b, connID, tx)
}

func (b *backLog) appendandWriteEvent(e *event.NormalizedEvent, idx int, tx *elasticapm.Transaction) {
	b.Lock()
	b.Directive.Rules[idx].Events = append(b.Directive.Rules[idx].Events, e.EventID)
	b.SrcIPs = str.AppendUniq(b.SrcIPs, e.SrcIP)
	b.DstIPs = str.AppendUniq(b.DstIPs, e.DstIP)
	b.Unlock()
	b.setStatusTime()
	if err := b.updateElasticsearch(e); err != nil {
		b.warn("failed to update Elasticsearch! "+err.Error(), e.ConnID)
		e := elasticapm.DefaultTracer.NewError(err)
		e.Transaction = tx
		e.Send()
		tx.Result = "Failed to append and write event"
	} else {
		tx.Result = "Event appended to backlog"
	}
	return
}

func (b backLog) isLastStage() (ret bool) {
	ret = b.CurrentStage == b.HighestStage
	return
}

func (b backLog) isStageReachMaxEvtCount(idx int) (reachMaxEvtCount bool) {
	currRule := b.Directive.Rules[idx]
	nEvents := len(b.Directive.Rules[idx].Events)
	if nEvents >= currRule.Occurrence {
		reachMaxEvtCount = true
	}
	return
}

func (b *backLog) setRuleEndTime(idx int) {
	b.Lock()
	b.Directive.Rules[idx].EndTime = time.Now().Unix()
	b.Unlock()
}

func (b *backLog) increaseStage(e *event.NormalizedEvent) error {
	b.Lock()
	n := int32(b.CurrentStage)
	b.CurrentStage = int(atomic.AddInt32(&n, 1))
	if b.CurrentStage > b.HighestStage {
		b.CurrentStage = b.HighestStage
	}
	idx := b.CurrentStage - 1
	t, err := time.Parse(time.RFC3339, e.Timestamp)
	if err != nil {
		b.Unlock()
		b.warn("cannot parse event timestamp "+e.Timestamp, e.ConnID)
		return err
	}
	b.Directive.Rules[idx].StartTime = t.Unix()
	b.StatusTime = time.Now().Unix()
	b.Unlock()
	b.info("stage increased", e.ConnID)
	return nil
}

func (b backLog) calcRisk(connID uint64) (riskChanged bool) {
	b.RLock()
	s := b.CurrentStage
	idx := s - 1
	from := b.Directive.Rules[idx].From
	to := b.Directive.Rules[idx].To
	value := asset.GetValue(from)
	tval := asset.GetValue(to)
	if tval > value {
		value = tval
	}

	pRisk := b.Risk

	reliability := b.Directive.Rules[idx].Reliability
	priority := b.Directive.Priority
	b.RUnlock()
	risk := priority * reliability * value / 25

	if risk != pRisk {
		b.Lock()
		b.Risk = risk
		b.Unlock()
		b.info("risk changed.", connID)
		riskChanged = true
	}
	return
}

// need to use ptr receiver for bLogs.delete
func (b *backLog) delete() {
	b.RLock()
	defer b.RUnlock()
	if b.deleted {
		return
	}
	b.debug("delete sending signal to bLogs", 0)
	// no need to do this, let it gc'ed
	// close(b.chFound)
	// close(b.chDone) // <-- closing twice causes panic, avoid this
	// b.chDone <- struct{}{}
	b.bLogs.delete(b)
}

func (b *backLog) setStatusTime() {
	b.Lock()
	b.StatusTime = time.Now().Unix()
	b.Unlock()
}

func (b backLog) updateElasticsearch(e *event.NormalizedEvent) error {
	log.Debug(log.M{Msg: "backlog updating Elasticsearch", DId: b.Directive.ID, BId: b.ID, CId: e.ConnID})
	b.StatusTime = time.Now().Unix()
	f, err := os.OpenFile(bLogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	v := siemAlarmEvents{b.ID, b.CurrentStage, e.EventID}
	vJSON, err := json.Marshal(v)
	if err != nil {
		fmt.Println(v)
		return err
	}
	_, err = f.WriteString(string(vJSON) + "\n") //race a
	return err
}
