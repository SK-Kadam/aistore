// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2021, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"fmt"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/mono"
	"github.com/NVIDIA/aistore/stats"
)

const (
	kaErrorMsg   = "error"
	kaStopMsg    = "stop"
	kaResumeMsg  = "resume"
	kaSuspendMsg = "suspend"

	kaNumRetries = 3
)

// interface guard
var (
	_ keepaliver = (*targetKeepalive)(nil)
	_ keepaliver = (*proxyKeepalive)(nil)
)

type keepaliver interface {
	onerr(err error, status int)
	heardFrom(sid string, reset bool)
	doKeepalive() (stopped bool)
	isTimeToPing(sid string) bool
	send(msg string)
	paused() bool
	cfg(config *cmn.Config) *cmn.KeepaliveTrackerConf
}

type targetKeepalive struct {
	t *targetrunner
	keepalive
}

type proxyKeepalive struct {
	p *proxyrunner
	keepalive
	stoppedCh  chan struct{}
	toRemoveCh chan string
}

type keepalive struct {
	name         string
	k            keepaliver
	kt           KeepaliveTracker
	tt           *timeoutTracker
	statsT       stats.Tracker
	controlCh    chan controlSignal
	inProgress   atomic.Int64 // A toggle used only by the primary proxy.
	startedUp    *atomic.Bool
	tickerPaused atomic.Bool

	// cached config
	maxKeepalive int64
	interval     time.Duration
}

type timeoutTracker struct {
	mu           sync.Mutex
	timeoutStats map[string]*timeoutStats
}

type timeoutStats struct {
	srtt    int64 // smoothed round-trip time in ns
	rttvar  int64 // round-trip time variation in ns
	timeout int64 // in ns
}

type controlSignal struct {
	msg string
	err error
}

// KeepaliveTracker defines the interface for keep alive tracking.
// It is safe for concurrent access.
type KeepaliveTracker interface {
	// HeardFrom notifies tracker that the server identified by 'id' has responded;
	// 'reset'=true when it is not a regular keepalive call (could be reconnect or re-register).
	HeardFrom(id string, reset bool)
	// TimedOut returns true if the 'id` server did not respond - an indication that the server
	// could be down
	TimedOut(id string) bool

	changed(factor uint8, interval time.Duration) bool
}

// interface guard
var (
	_ cos.Runner = (*targetKeepalive)(nil)
	_ cos.Runner = (*proxyKeepalive)(nil)
)

/////////////////////
// targetKeepalive //
/////////////////////

func newTargetKeepalive(t *targetrunner, statsT stats.Tracker, startedUp *atomic.Bool) *targetKeepalive {
	config := cmn.GCO.Get()

	tkr := &targetKeepalive{t: t}
	tkr.keepalive.name = "targetkeepalive"
	tkr.keepalive.k = tkr
	tkr.statsT = statsT
	tkr.keepalive.startedUp = startedUp
	tkr.kt = newKeepaliveTracker(&config.Keepalive.Target)
	tkr.tt = &timeoutTracker{timeoutStats: make(map[string]*timeoutStats, 8)}
	tkr.controlCh = make(chan controlSignal) // unbuffered on purpose
	tkr.interval = config.Keepalive.Target.Interval.D()
	tkr.maxKeepalive = int64(config.Timeout.MaxKeepalive)
	return tkr
}

func (*targetKeepalive) cfg(config *cmn.Config) *cmn.KeepaliveTrackerConf {
	return &config.Keepalive.Target
}

func (tkr *targetKeepalive) doKeepalive() (stopped bool) {
	smap := tkr.t.owner.smap.get()
	if smap == nil || smap.validate() != nil {
		return
	}
	if stopped = tkr.register(tkr.t.sendKeepalive, smap.Primary.ID(), tkr.t.si.Name()); stopped {
		tkr.t.onPrimaryFail()
	}
	return
}

////////////////////
// proxyKeepalive //
////////////////////

func newProxyKeepalive(p *proxyrunner, statsT stats.Tracker, startedUp *atomic.Bool) *proxyKeepalive {
	config := cmn.GCO.Get()

	pkr := &proxyKeepalive{p: p}
	pkr.keepalive.name = "proxykeepalive"
	pkr.keepalive.k = pkr
	pkr.statsT = statsT
	pkr.keepalive.startedUp = startedUp
	pkr.kt = newKeepaliveTracker(&config.Keepalive.Proxy)
	pkr.tt = &timeoutTracker{timeoutStats: make(map[string]*timeoutStats, 8)}
	pkr.controlCh = make(chan controlSignal) // unbuffered on purpose
	pkr.interval = config.Keepalive.Proxy.Interval.D()
	pkr.maxKeepalive = int64(config.Timeout.MaxKeepalive)
	return pkr
}

func (*proxyKeepalive) cfg(config *cmn.Config) *cmn.KeepaliveTrackerConf {
	return &config.Keepalive.Proxy
}

func (pkr *proxyKeepalive) doKeepalive() (stopped bool) {
	smap := pkr.p.owner.smap.get()
	if smap == nil || smap.validate() != nil {
		return
	}
	if smap.isPrimary(pkr.p.si) {
		return pkr.updateSmap()
	}
	if !pkr.isTimeToPing(smap.Primary.ID()) {
		return
	}
	if stopped = pkr.register(pkr.p.sendKeepalive, smap.Primary.ID(), pkr.p.si.Name()); stopped {
		pkr.p.onPrimaryFail()
	}
	return
}

// updateSmap pings all nodes in parallel. Non-responding nodes get removed from the Smap and
// the resulting map is then metasync-ed.
func (pkr *proxyKeepalive) updateSmap() (stopped bool) {
	if !pkr.inProgress.CAS(0, 1) {
		glog.Infof("%s: primary keepalive is in progress...", pkr.p.si)
		return
	}
	defer pkr.inProgress.CAS(1, 0)
	var (
		p         = pkr.p
		smap      = p.owner.smap.get()
		daemonCnt = smap.Count()
	)
	pkr.openCh(daemonCnt)
	// limit parallelism, here and elsewhere
	wg := cos.NewLimitedWaitGroup(cluster.MaxBcastParallel(), daemonCnt)
	for _, daemons := range []cluster.NodeMap{smap.Tmap, smap.Pmap} {
		for sid, si := range daemons {
			if sid == p.si.ID() {
				continue
			}
			// skipping
			if !pkr.isTimeToPing(sid) {
				continue
			}
			if si.IsAnySet(cluster.NodeFlagsMaintDecomm) {
				continue
			}
			// do keepalive
			wg.Add(1)
			go pkr.do(si, wg)
		}
	}
	wg.Wait()
	if stopped = len(pkr.stoppedCh) > 0; stopped {
		pkr.closeCh()
		return
	}
	if len(pkr.toRemoveCh) == 0 {
		return
	}
	ctx := &smapModifier{pre: pkr._pre, final: pkr._final}
	err := p.owner.smap.modify(ctx)
	if err != nil {
		if ctx.msg != nil {
			glog.Errorf("FATAL: %v", err)
		} else {
			glog.Warning(err)
		}
	}
	return
}

func (pkr *proxyKeepalive) do(si *cluster.Snode, wg cos.WG) {
	defer wg.Done()
	if len(pkr.stoppedCh) > 0 {
		return
	}
	ok, stopped := pkr._pingRetry(si)
	if stopped {
		pkr.stoppedCh <- struct{}{}
	}
	if !ok {
		pkr.toRemoveCh <- si.ID()
	}
}

func (pkr *proxyKeepalive) _pingRetry(to *cluster.Snode) (ok, stopped bool) {
	var (
		timeout        = time.Duration(pkr.timeoutStatsForDaemon(to.ID()).timeout)
		t              = mono.NanoTime()
		_, status, err = pkr.p.Health(to, timeout, nil)
	)
	delta := mono.Since(t)
	pkr.updateTimeoutForDaemon(to.ID(), delta)
	pkr.statsT.Add(stats.KeepAliveLatency, int64(delta))

	if err == nil {
		return true, false
	}
	glog.Warningf("%s fails to respond, err: %v(%d) - retrying...", to.StringEx(), err, status)
	ok, stopped = pkr.retry(to)
	return ok, stopped
}

func (pkr *proxyKeepalive) openCh(daemonCnt int) {
	if pkr.stoppedCh == nil || cap(pkr.stoppedCh) < daemonCnt {
		pkr.stoppedCh = make(chan struct{}, daemonCnt*2)
		pkr.toRemoveCh = make(chan string, daemonCnt*2)
	}
	debug.Assert(len(pkr.stoppedCh) == 0)
	debug.Assert(len(pkr.toRemoveCh) == 0)
}

func (pkr *proxyKeepalive) closeCh() {
	close(pkr.stoppedCh)
	close(pkr.toRemoveCh)
	pkr.stoppedCh, pkr.toRemoveCh = nil, nil
}

func (pkr *proxyKeepalive) _pre(ctx *smapModifier, clone *smapX) error {
	ctx.smap = pkr.p.owner.smap.get()
	if !ctx.smap.isPrimary(pkr.p.si) {
		return newErrNotPrimary(pkr.p.si, ctx.smap)
	}
	metaction := "keepalive: removing ["
	cnt := 0
loop:
	for {
		select {
		case sid := <-pkr.toRemoveCh:
			metaction += " ["
			if clone.GetProxy(sid) != nil {
				clone.delProxy(sid)
				clone.staffIC()
				metaction += cmn.Proxy
				cnt++
			} else if clone.GetTarget(sid) != nil {
				clone.delTarget(sid)
				metaction += cmn.Target
				cnt++
			} else {
				metaction += unknownDaemonID
				glog.Warningf("%s not present in the %s (old %s)", sid, clone, ctx.smap)
			}
			metaction += ":" + sid + "] "

			// Remove reverse proxy entry for the node.
			pkr.p.rproxy.nodes.Delete(sid)
		default:
			break loop
		}
	}
	metaction += "]"
	if cnt == 0 {
		return fmt.Errorf("%s: nothing to do [%s, %s]", pkr.p.si, ctx.smap.StringEx(), metaction)
	}
	ctx.msg = &cmn.ActionMsg{Value: metaction}
	return nil
}

func (pkr *proxyKeepalive) _final(ctx *smapModifier, clone *smapX) {
	msg := pkr.p.newAmsg(ctx.msg, nil)
	debug.Assert(clone._sgl != nil)
	_ = pkr.p.metasyncer.sync(revsPair{clone, msg})
}

func (pkr *proxyKeepalive) retry(si *cluster.Snode) (ok, stopped bool) {
	var (
		timeout = time.Duration(pkr.timeoutStatsForDaemon(si.ID()).timeout)
		ticker  = time.NewTicker(cmn.KeepaliveRetryDuration())
		i       int
	)
	defer ticker.Stop()
	for {
		if !pkr.isTimeToPing(si.ID()) {
			return true, false
		}
		select {
		case <-ticker.C:
			t := mono.NanoTime()
			_, status, err := pkr.p.Health(si, timeout, nil)
			timeout = pkr.updateTimeoutForDaemon(si.ID(), mono.Since(t))
			if err == nil {
				return true, false
			}
			i++
			if i == kaNumRetries {
				smap := pkr.p.owner.smap.get()
				sname := si.StringEx()
				glog.Warningf("Failed to keepalive %s after %d attempts - removing %s from the %s",
					sname, i, sname, smap)
				return false, false
			}
			if cos.IsUnreachable(err, status) {
				continue
			}
			glog.Warningf("Unexpected error %v(%d) from %s", err, status, si.StringEx())
		case sig := <-pkr.controlCh:
			if sig.msg == kaStopMsg {
				return false, true
			}
		}
	}
}

///////////////
// keepalive //
///////////////

func (k *keepalive) Name() string { return k.name }

func (k *keepalive) waitStatsRunner() (stopped bool) {
	const (
		waitSelfJoin = 300 * time.Millisecond
		waitStandby  = 5 * time.Second
	)
	var (
		logErr time.Duration
		ticker *time.Ticker
	)
	if daemon.cli.target.standby {
		ticker = time.NewTicker(waitStandby)
	} else {
		ticker = time.NewTicker(waitSelfJoin)
	}
	defer ticker.Stop()

	// Wait for stats runner to start
	for {
		select {
		case <-ticker.C:
			if k.startedUp.Load() {
				return false
			}
			logErr += waitSelfJoin
			config := cmn.GCO.Get()
			if logErr > config.Timeout.Startup.D() {
				glog.Errorln("startup is taking unusually long time...")
				logErr = 0
			}
		case sig := <-k.controlCh:
			switch sig.msg {
			case kaStopMsg:
				return true
			default:
			}
		}
	}
}

func (k *keepalive) Run() error {
	if k.waitStatsRunner() {
		return nil // Stopped while waiting - must exit.
	}
	glog.Infof("Starting %s", k.Name())
	var (
		ticker    = time.NewTicker(k.interval)
		lastCheck int64
	)
	k.tickerPaused.Store(false)
	for {
		select {
		case <-ticker.C:
			lastCheck = mono.NanoTime()
			k.k.doKeepalive()
			config := cmn.GCO.Get()
			k.configUpdate(config.Timeout.MaxKeepalive.D(), k.k.cfg(config))
		case sig := <-k.controlCh:
			switch sig.msg {
			case kaResumeMsg:
				ticker.Reset(k.interval)
				k.tickerPaused.Store(false)
			case kaSuspendMsg:
				ticker.Stop()
				k.tickerPaused.Store(true)
			case kaStopMsg:
				ticker.Stop()
				return nil
			case kaErrorMsg:
				if mono.Since(lastCheck) >= cmn.KeepaliveRetryDuration() {
					lastCheck = mono.NanoTime()
					glog.Infof("triggered by %v", sig.err)
					if stopped := k.k.doKeepalive(); stopped {
						ticker.Stop()
						return nil
					}
				}
			}
		}
	}
}

func (k *keepalive) configUpdate(maxKeepalive time.Duration, cfg *cmn.KeepaliveTrackerConf) {
	k.maxKeepalive = int64(maxKeepalive)
	if !k.kt.changed(cfg.Factor, cfg.Interval.D()) {
		return
	}
	k.interval = cfg.Interval.D()
	k.kt = newKeepaliveTracker(cfg)
}

// register is called by non-primary proxies and targets to send a keepalive to the primary proxy.
func (k *keepalive) register(sendKeepalive func(time.Duration) (int, error), primaryID, hname string) (stopped bool) {
	var (
		timeout     = time.Duration(k.timeoutStatsForDaemon(primaryID).timeout)
		now         = mono.NanoTime()
		status, err = sendKeepalive(timeout)
		delta       = mono.SinceNano(now)
		pname       = "primary[" + primaryID + "]"
	)
	k.statsT.Add(stats.KeepAliveLatency, delta)
	if err == nil {
		return
	}
	glog.Warningf("%s => %s keepalive failed: %v(%d)", hname, pname, err, status)
	var (
		ticker = time.NewTicker(cmn.KeepaliveRetryDuration())
		i      int
	)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			i++
			now = mono.NanoTime()
			status, err = sendKeepalive(timeout)
			delta := mono.Since(now)
			// In case the error is some kind of connection error, the round-trip
			// could be much shorter than the specified `timeout`. In such case
			// we want to report the worst-case scenario, otherwise we could possibly
			// decrease next retransmission timeout (which doesn't make much sense).
			if cos.IsRetriableConnErr(err) {
				delta = time.Duration(k.maxKeepalive)
			}
			timeout = k.updateTimeoutForDaemon(primaryID, delta)
			if err == nil {
				glog.Infof("%s: OK after %d attempt%s", hname, i, cos.Plural(i))
				return
			}
			if i == kaNumRetries {
				glog.Warningf("%s: failed to keepalive with %s after %d attempts", hname, pname, i)
				return true
			}
			if cos.IsUnreachable(err, status) {
				continue
			}
			if daemon.stopping.Load() {
				return true
			}
			err = fmt.Errorf("%s: unexpected response from %s: %v(%d)", hname, pname, err, status)
			debug.AssertNoErr(err)
			glog.Warning(err)
		case sig := <-k.controlCh:
			if sig.msg == kaStopMsg {
				return true
			}
		}
	}
}

// updateTimeoutForDaemon calculates the new timeout for the daemon with ID sid, updates it in
// k.timeoutStatsForDaemon, and returns it. The algorithm is loosely based on TCP's RTO calculation,
// as documented in RFC 6298.
func (k *keepalive) updateTimeoutForDaemon(sid string, d time.Duration) time.Duration {
	const (
		alpha = 125
		beta  = 250
		c     = 4
	)
	next := int64(d)
	ts := k.timeoutStatsForDaemon(sid)
	ts.rttvar = (1000-beta)*ts.rttvar + beta*(cos.AbsI64(ts.srtt-next))
	ts.rttvar = cos.DivRound(ts.rttvar, 1000)
	ts.srtt = (1000-alpha)*ts.srtt + alpha*next
	ts.srtt = cos.DivRound(ts.srtt, 1000)
	ts.timeout = cos.MinI64(k.maxKeepalive, ts.srtt+c*ts.rttvar)
	ts.timeout = cos.MaxI64(ts.timeout, k.maxKeepalive/2)
	return time.Duration(ts.timeout)
}

// timeoutStatsForDaemon returns the timeoutStats corresponding to daemon ID sid.
// If there is no entry in k.timeoutStats for sid, then the initial timeout will be set to
// maxKeepaliveNS, with the other stats loosely based on RFC 6298.
func (k *keepalive) timeoutStatsForDaemon(sid string) *timeoutStats {
	k.tt.mu.Lock()
	if ts := k.tt.timeoutStats[sid]; ts != nil {
		k.tt.mu.Unlock()
		return ts
	}
	ts := &timeoutStats{srtt: k.maxKeepalive, rttvar: k.maxKeepalive / 2, timeout: k.maxKeepalive}
	k.tt.timeoutStats[sid] = ts
	k.tt.mu.Unlock()
	return ts
}

func (k *keepalive) onerr(err error, status int) {
	if cos.IsUnreachable(err, status) {
		k.controlCh <- controlSignal{msg: kaErrorMsg, err: err}
	}
}

func (k *keepalive) heardFrom(sid string, reset bool) {
	k.kt.HeardFrom(sid, reset)
}

func (k *keepalive) isTimeToPing(sid string) bool {
	return k.kt.TimedOut(sid)
}

func (k *keepalive) Stop(err error) {
	glog.Infof("Stopping %s, err: %v", k.Name(), err)
	k.controlCh <- controlSignal{msg: kaStopMsg}
	close(k.controlCh)
}

func (k *keepalive) send(msg string) {
	glog.Infof("Sending %q on the control channel", msg)
	k.controlCh <- controlSignal{msg: msg}
}

func (k *keepalive) paused() bool { return k.tickerPaused.Load() }

///////////////
// HBTracker //
///////////////

// HBTracker tracks the timestamp of the last time a message is received from a server.
// Timeout: a message is not received within the interval.
type HBTracker struct {
	mtx      sync.RWMutex
	last     map[string]int64
	interval time.Duration // expected to hear from the server within the interval
}

// interface guard
var (
	_ KeepaliveTracker = (*HBTracker)(nil)
)

// NewKeepaliveTracker returns a keepalive tracker based on the parameters given.
func newKeepaliveTracker(c *cmn.KeepaliveTrackerConf) KeepaliveTracker {
	switch c.Name {
	case cmn.KeepaliveHeartbeatType:
		return newHBTracker(c.Interval.D())
	case cmn.KeepaliveAverageType:
		return newAvgTracker(c.Factor)
	}
	return nil
}

// newHBTracker returns a HBTracker.
func newHBTracker(interval time.Duration) *HBTracker {
	return &HBTracker{
		last:     make(map[string]int64),
		interval: interval,
	}
}

func (hb *HBTracker) HeardFrom(id string, _ bool) {
	hb.mtx.Lock()
	hb.last[id] = mono.NanoTime()
	hb.mtx.Unlock()
}

func (hb *HBTracker) TimedOut(id string) bool {
	hb.mtx.RLock()
	t, ok := hb.last[id]
	hb.mtx.RUnlock()
	return !ok || mono.Since(t) > hb.interval
}

func (hb *HBTracker) changed(_ uint8, interval time.Duration) bool {
	return hb.interval != interval
}

////////////////
// AvgTracker //
////////////////

// AvgTracker keeps track of the average latency of all messages.
// Timeout: last received is more than the 'factor' of current average.
type (
	AvgTracker struct {
		mtx    sync.RWMutex
		rec    map[string]avgTrackerRec
		factor uint8
	}
	avgTrackerRec struct {
		count   int64
		last    int64
		totalMS int64 // in ms
	}
)

// interface guard
var (
	_ KeepaliveTracker = (*AvgTracker)(nil)
)

func (rec *avgTrackerRec) avg() int64 {
	return rec.totalMS / rec.count
}

// newAvgTracker returns an AvgTracker.
func newAvgTracker(factor uint8) *AvgTracker {
	return &AvgTracker{rec: make(map[string]avgTrackerRec), factor: factor}
}

// HeardFrom is called to indicate that a keepalive message (or equivalent) has been received.
func (a *AvgTracker) HeardFrom(id string, reset bool) {
	var rec avgTrackerRec
	a.mtx.Lock()
	defer a.mtx.Unlock()
	rec, ok := a.rec[id]
	if reset || !ok {
		a.rec[id] = avgTrackerRec{count: 0, totalMS: 0, last: mono.NanoTime()}
		return
	}
	t := mono.NanoTime()
	delta := t - rec.last
	rec.last = t
	rec.count++
	rec.totalMS += delta / int64(time.Millisecond)
	a.rec[id] = rec
}

func (a *AvgTracker) TimedOut(id string) bool {
	a.mtx.RLock()
	rec, ok := a.rec[id]
	a.mtx.RUnlock()
	if !ok {
		return true
	}
	if rec.count == 0 {
		return false
	}
	return int64(mono.Since(rec.last)/time.Millisecond) > int64(a.factor)*rec.avg()
}

func (a *AvgTracker) changed(factor uint8, _ time.Duration) bool {
	return a.factor != factor
}
