// Copyright 2017-2021 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package resolve

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"sync"
	"time"

	"github.com/caffix/queue"
	"github.com/miekg/dns"
	"go.uber.org/ratelimit"
)

const (
	maxDelayBetweenSamples time.Duration = 250 * time.Millisecond
	minSamplingTime        time.Duration = 5 * time.Second
	minSampleSetSize       int           = 5
)

type baseResolver struct {
	sync.Mutex
	stopped bool
	done    chan struct{}
	// Rate limiter to enforce the maximum DNS queries
	ratelock         sync.Mutex
	rlimit           ratelimit.Limiter
	measure          chan time.Time
	xchgQueue        queue.Queue
	xchgs            *xchgManager
	readMsgs         queue.Queue
	wildcardChannels *wildcardChans
	address          string
	log              *log.Logger
	perSec           int
	conn             *dns.Conn
}

// NewBaseResolver initializes a Resolver that sends DNS queries to the provided IP address.
func NewBaseResolver(addr string, perSec int, logger *log.Logger) Resolver {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		// Add the default port number to the IP address
		addr = net.JoinHostPort(addr, "53")
	}

	if perSec <= 0 {
		return nil
	}

	// Assign a null logger when one is not provided
	if logger == nil {
		logger = log.New(ioutil.Discard, "", 0)
	}

	c := dns.Client{UDPSize: dns.DefaultMsgSize}
	conn, err := c.Dial(addr)
	if err != nil {
		logger.Printf("Failed to establish a UDP connection to %s : %v", addr, err)
		return nil
	}

	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		logger.Printf("Failed to clear the read deadline for the UDP connection to %s : %v", addr, err)
		return nil
	}

	r := &baseResolver{
		done:      make(chan struct{}, 2),
		rlimit:    ratelimit.New(perSec, ratelimit.WithoutSlack),
		measure:   make(chan time.Time, 2),
		xchgQueue: queue.NewQueue(),
		xchgs:     newXchgManager(),
		readMsgs:  queue.NewQueue(),
		wildcardChannels: &wildcardChans{
			WildcardReq:     queue.NewQueue(),
			IPsAcrossLevels: make(chan *ipsAcrossLevels, 10),
			TestResult:      make(chan *testResult, 10),
		},
		address: addr,
		log:     logger,
		perSec:  perSec,
		conn:    conn,
	}

	go r.manageWildcards(r.wildcardChannels)
	go r.sendQueries()
	go r.responses()
	go r.timeouts()
	go r.handleReads()
	return r
}

// Stop implements the Resolver interface.
func (r *baseResolver) Stop() {
	r.Lock()
	defer r.Unlock()

	if !r.stopped {
		close(r.done)
	}

	r.stopped = true
}

// Stopped implements the Resolver interface.
func (r *baseResolver) Stopped() bool {
	r.Lock()
	defer r.Unlock()

	return r.stopped
}

// String implements the Stringer interface.
func (r *baseResolver) String() string {
	return r.address
}

func (r *baseResolver) rateLimiterTake() {
	r.ratelock.Lock()
	defer r.ratelock.Unlock()

	r.rlimit.Take()
}

func (r *baseResolver) setRateLimit(perSec int) {
	r.ratelock.Lock()
	defer r.ratelock.Unlock()

	r.rlimit = ratelimit.New(perSec, ratelimit.WithoutSlack)
}

// Query implements the Resolver interface.
func (r *baseResolver) Query(ctx context.Context, msg *dns.Msg, priority int, retry Retry) (*dns.Msg, error) {
	if priority != PriorityCritical && priority != PriorityHigh &&
		priority != PriorityNormal && priority != PriorityLow {
		return nil, &ResolveError{
			Err:   fmt.Sprintf("Resolver: Invalid priority parameter: %d", priority),
			Rcode: ResolverErrRcode,
		}
	}

	if r.Stopped() {
		return nil, &ResolveError{
			Err:   fmt.Sprintf("Resolver: %s has been stopped", r.String()),
			Rcode: ResolverErrRcode,
		}
	}

	again := true
	var times int
	var err error
	var resp *dns.Msg
	for again {
		err = checkContext(ctx)
		if err != nil {
			break
		}

		times++
		result := r.queueQuery(ctx, msg, priority)
		resp = result.Msg
		err = result.Err
		if err == nil || retry == nil {
			break
		}

		resp := result.Msg
		rcode := (result.Err.(*ResolveError)).Rcode
		if resp == nil {
			resp = msg
			resp.Rcode = rcode
		}
		again = retry(times, priority, resp)
	}

	return resp, err
}

func (r *baseResolver) queueQuery(ctx context.Context, msg *dns.Msg, p int) *resolveResult {
	resultChan := make(chan *resolveResult, 2)

	priority := queue.PriorityNormal
	switch p {
	case PriorityCritical:
		priority = queue.PriorityCritical
	case PriorityHigh:
		priority = queue.PriorityHigh
	case PriorityLow:
		priority = queue.PriorityLow
	}

	req := &resolveRequest{
		ID:     msg.Id,
		Name:   RemoveLastDot(msg.Question[0].Name),
		Qtype:  msg.Question[0].Qtype,
		Msg:    msg,
		Result: resultChan,
	}

	if err := r.xchgs.add(req); err != nil {
		estr := fmt.Sprintf("Failed to obtain a valid message identifier: %v", err)
		return makeResolveResult(nil, true, estr, ResolverErrRcode)
	}
	r.xchgQueue.AppendPriority(req, priority)

	var result *resolveResult
	select {
	case <-ctx.Done():
		result = makeResolveResult(nil, false, "The request context was cancelled", TimeoutRcode)
	case res := <-resultChan:
		result = res
	}
	return result
}

func (r *baseResolver) sendQueries() {
	var last time.Time
	var measuring bool

	for {
		select {
		case <-r.done:
			return
		case <-r.xchgQueue.Signal():
			if element, ok := r.xchgQueue.Next(); ok {
				r.rateLimiterTake()

				now := time.Now()
				if l := r.xchgQueue.Len(); !measuring && l > 0 {
					r.measure <- now
					measuring = true
				} else if measuring && l == 0 && now.Sub(last) > maxDelayBetweenSamples {
					r.measure <- time.Time{}
					measuring = false
				}

				last = now
				r.writeMessage(element.(*resolveRequest))
			}
		}
	}
}

func (r *baseResolver) writeMessage(req *resolveRequest) {
	if err := r.conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		estr := fmt.Sprintf("Failed to set the write deadline: %v", err)

		r.xchgs.remove(req.ID, req.Name)
		r.returnRequest(req, makeResolveResult(nil, true, estr, TimeoutRcode))
		return
	}

	if err := r.conn.WriteMsg(req.Msg); err != nil {
		estr := fmt.Sprintf("Failed to write the query msg: %v", err)

		r.xchgs.remove(req.ID, req.Name)
		r.returnRequest(req, makeResolveResult(nil, true, estr, TimeoutRcode))
		return
	}

	// Set the timestamp for message expiration
	r.xchgs.updateTimestamp(req.ID, req.Name)
}

func (r *baseResolver) timeouts() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
loop:
	for {
		select {
		case <-r.done:
			break loop
		case <-t.C:
			for _, req := range r.xchgs.removeExpired() {
				if req.Msg != nil {
					estr := fmt.Sprintf("Query on resolver %s, for %s type %d timed out",
						r.address, req.Name, req.Qtype)
					r.returnRequest(req, makeResolveResult(nil, true, estr, TimeoutRcode))
				}
			}
		}
	}
	// Drains the xchgs of all messages and allows callers to return
	for _, req := range r.xchgs.removeAll() {
		if req.Msg != nil {
			estr := fmt.Sprintf("Resolver %s has stopped", r.address)
			r.returnRequest(req, makeResolveResult(nil, false, estr, ResolverErrRcode))
		}
	}
}

type readMsg struct {
	Req  *resolveRequest
	Resp *dns.Msg
}

func (r *baseResolver) responses() {
	var responseTimes []time.Time
	var collect, stopped, last time.Time

	update := func() {
		times := make([]time.Time, len(responseTimes))
		copy(times, responseTimes)
		go r.calcNewRate(times)
		responseTimes = []time.Time{}
		last = time.Now()
	}

	for {
		select {
		case <-r.done:
			return
		case t := <-r.measure:
			collect = t
			if collect.IsZero() {
				stopped = time.Now()
			} else {
				update()
			}
		default:
		}

		if m, err := r.conn.ReadMsg(); err == nil && m != nil && len(m.Question) > 0 {
			rtime := time.Now()

			if req := r.xchgs.remove(m.Id, m.Question[0].Name); req != nil {
				var add bool

				// Collect resolver response times
				if !collect.IsZero() && req.Timestamp.After(collect) {
					add = true
				} else if collect.IsZero() && req.Timestamp.Before(stopped) {
					add = true
				} else if len(responseTimes) > minSampleSetSize && rtime.Sub(last) > minSamplingTime {
					update()
				}
				if add {
					responseTimes = append(responseTimes, rtime)
				}

				r.readMsgs.Append(&readMsg{
					Req:  req,
					Resp: m,
				})
			}
		}
	}
}

func (r *baseResolver) calcNewRate(times []time.Time) {
	var last time.Time
	var total time.Duration

	if len(times) < minSampleSetSize {
		return
	}

	for i, t := range times {
		if i > 0 {
			total += t.Sub(last)
		}
		last = t
	}

	avg := total / time.Duration(len(times)-1)
	if avg > time.Second {
		avg = time.Second
	}
	avg -= time.Duration(float64(avg) * 0.25)

	persec := int(time.Second / avg)
	if persec <= 1 {
		persec = r.perSec
	}
	r.setRateLimit(persec)
}

func (r *baseResolver) handleReads() {
	each := func(element interface{}) {
		if read, ok := element.(*readMsg); ok {
			r.processMessage(read.Resp, read.Req)
		}
	}
loop:
	for {
		select {
		case <-r.done:
			break loop
		case <-r.readMsgs.Signal():
			if e, ok := r.readMsgs.Next(); ok {
				each(e)
			}
		}
	}
	// Drains the queue of all messages and allows callers to return
	r.readMsgs.Process(each)
}

func (r *baseResolver) processMessage(m *dns.Msg, req *resolveRequest) {
	// Check that the query was successful
	if m.Rcode != dns.RcodeSuccess {
		var again bool

		for _, code := range RetryCodes {
			if m.Rcode == code {
				again = true
				break
			}
		}

		estr := fmt.Sprintf("Query on resolver %s, for %s type %d returned error %s",
			r.address, req.Name, req.Qtype, dns.RcodeToString[m.Rcode])
		r.returnRequest(req, makeResolveResult(m, again, estr, m.Rcode))
		return
	}

	if m.Truncated {
		go r.tcpExchange(req)
		return
	}

	r.returnRequest(req, &resolveResult{
		Msg:   m,
		Again: false,
		Err:   nil,
	})
}

func (r *baseResolver) tcpExchange(req *resolveRequest) {
	client := dns.Client{
		Net:     "tcp",
		Timeout: time.Minute,
	}

	m, _, err := client.Exchange(req.Msg, r.address)
	if err != nil {
		estr := fmt.Sprintf("Failed to perform the exchange via TCP to %s: %v", r.address, err)
		r.returnRequest(req, makeResolveResult(nil, true, estr, ResolverErrRcode))
		return
	}

	r.returnRequest(req, &resolveResult{
		Msg:   m,
		Again: false,
		Err:   nil,
	})
}
