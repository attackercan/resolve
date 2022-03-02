// Copyright © by Jeff Foley 2017-2022. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"strings"
	"sync"

	"github.com/caffix/stringset"
	"github.com/miekg/dns"
)

// Constants related to DNS labels.
const (
	MaxDNSNameLen  = 253
	MaxDNSLabelLen = 63
	MinLabelLen    = 6
	MaxLabelLen    = 24
	LDHChars       = "abcdefghijklmnopqrstuvwxyz0123456789-"
)

const (
	numOfWildcardTests int = 3
	maxQueryAttempts   int = 5
)

var wildcardQueryTypes = []uint16{
	dns.TypeCNAME,
	dns.TypeA,
	dns.TypeAAAA,
}

type wildcard struct {
	sync.Mutex
	Detected bool
	Answers  []*ExtractedAnswer
}

// UnlikelyName takes a subdomain name and returns an unlikely DNS name within that subdomain.
func UnlikelyName(sub string) string {
	ldh := []rune(LDHChars)
	ldhLen := len(ldh)

	// Determine the max label length
	l := MaxDNSNameLen - (len(sub) + 1)
	if l > MaxLabelLen {
		l = MaxLabelLen
	} else if l < MinLabelLen {
		l = MinLabelLen
	}
	// Shuffle our LDH characters
	rand.Shuffle(ldhLen, func(i, j int) {
		ldh[i], ldh[j] = ldh[j], ldh[i]
	})

	var newlabel string
	l = MinLabelLen + rand.Intn((l-MinLabelLen)+1)
	for i := 0; i < l; i++ {
		sel := rand.Int() % (ldhLen - 1)
		newlabel = newlabel + string(ldh[sel])
	}

	newlabel = strings.Trim(newlabel, "-")
	if newlabel == "" {
		return newlabel
	}
	return newlabel + "." + sub
}

// WildcardDetected returns true when the provided DNS response could be a wildcard match.
func (r *Resolvers) WildcardDetected(ctx context.Context, resp *dns.Msg, domain string) bool {
	if !r.goodDetector() {
		return false
	}

	name := strings.ToLower(RemoveLastDot(resp.Question[0].Name))
	domain = strings.ToLower(RemoveLastDot(domain))

	base := len(strings.Split(domain, "."))
	labels := strings.Split(name, ".")
	if len(labels) > base {
		labels = labels[1:]
	}
	// Check for a DNS wildcard at each label starting with the root domain
	for i := len(labels) - base; i >= 0; i-- {
		sub := strings.Join(labels[i:], ".")

		w := r.getWildcard(sub)
		if w == nil {
			w = &wildcard{}
			w.Lock()
			r.setWildcard(sub, w)
			w.Detected, w.Answers = r.wildcardTest(ctx, sub)
			w.Unlock()
		}

		w.Lock()
		match := w.respMatchesWildcard(resp)
		w.Unlock()

		if match {
			return true
		}
	}
	return r.testIPsAcrossLevels(name, domain)
}

// SetDetectionResolver sets the provided DNS resolver as responsible for wildcard detection.
func (r *Resolvers) SetDetectionResolver(qps int, addr string) error {
	if err := r.AddResolvers(qps, addr); err != nil {
		return err
	}

	if _, _, err := net.SplitHostPort(addr); err != nil {
		// Add the default port number to the IP address
		addr = net.JoinHostPort(addr, "53")
	}

	r.Lock()
	defer r.Unlock()

	if res := r.searchList(addr); res != nil {
		r.detector = res
		return nil
	}
	return errors.New("failed to add the wildcard detection resolver")
}

func (r *Resolvers) getDetectionResolver() *resolver {
	r.Lock()
	defer r.Unlock()

	return r.detector
}

func (r *Resolvers) getWildcard(sub string) *wildcard {
	r.Lock()
	defer r.Unlock()

	return r.wildcards[sub]
}

func (r *Resolvers) setWildcard(sub string, w *wildcard) {
	r.Lock()
	defer r.Unlock()

	r.wildcards[sub] = w
}

func (r *Resolvers) goodDetector() bool {
	if d := r.getDetectionResolver(); d == nil {
		d = r.randResolver()
		if d == nil {
			return false
		}

		if err := r.SetDetectionResolver(d.qps, d.address); err != nil {
			return false
		}
	}
	return true
}

func (w *wildcard) respMatchesWildcard(resp *dns.Msg) bool {
	if w.Detected {
		if len(w.Answers) == 0 || len(resp.Answer) == 0 {
			return w.Detected
		}

		set := stringset.New()
		defer set.Close()

		insertRecordData(set, ExtractAnswers(resp))
		intersectRecordData(set, w.Answers)
		if set.Len() > 0 {
			return w.Detected
		}
	}
	return false
}

func (r *Resolvers) wildcardTest(ctx context.Context, sub string) (bool, []*ExtractedAnswer) {
	var detected bool
	var answers []*ExtractedAnswer

	set := stringset.New()
	defer set.Close()

	// Query multiple times with unlikely names against this subdomain
	for i := 0; i < numOfWildcardTests; i++ {
		var name string
		// Generate the unlikely label / name
		for {
			name = UnlikelyName(sub)
			if name != "" {
				break
			}
		}

		var ans []*ExtractedAnswer
		for _, t := range wildcardQueryTypes {
			if a := r.makeQueryAttempts(ctx, name, t); len(a) > 0 {
				detected = true
				ans = append(ans, a...)
			}
		}

		if i == 0 {
			insertRecordData(set, ans)
		} else {
			intersectRecordData(set, ans)
		}
		answers = append(answers, ans...)
	}

	already := stringset.New()
	defer already.Close()

	var final []*ExtractedAnswer
	// Create the slice of answers common across all the unlikely name queries
	for _, a := range answers {
		a.Data = strings.Trim(a.Data, ".")

		if set.Has(a.Data) && !already.Has(a.Data) {
			final = append(final, a)
			already.Insert(a.Data)
		}
	}
	// Determine whether the subdomain has a DNS wildcard, and if so, which type is it?
	if detected {
		r.log.Printf("DNS wildcard detected: Resolver %s: %s", r.detector.address, "*."+sub)
	}
	return detected, final
}

func (r *Resolvers) makeQueryAttempts(ctx context.Context, name string, qtype uint16) []*ExtractedAnswer {
	ch := make(chan *dns.Msg, 2)
	detector := r.getDetectionResolver()

	for i := 0; i < maxQueryAttempts; i++ {
		msg := QueryMsg(name, qtype)
		req := &resolveRequest{
			Ctx:    ctx,
			ID:     msg.Id,
			Name:   RemoveLastDot(msg.Question[0].Name),
			Qtype:  msg.Question[0].Qtype,
			Msg:    msg,
			Result: ch,
		}

		detector.query(req)
		if resp := <-ch; resp.Rcode == dns.RcodeSuccess && len(resp.Answer) > 0 {
			return ExtractAnswers(resp)
		}
	}
	return nil
}

func (r *Resolvers) testIPsAcrossLevels(name, domain string) bool {
	base := len(strings.Split(domain, "."))
	labels := strings.Split(strings.ToLower(name), ".")
	if len(labels) <= base || (len(labels)-base) < 3 {
		return false
	}

	l := len(labels) - base
	records := stringset.New()
	defer records.Close()

	for i := 1; i <= l; i++ {
		w := r.getWildcard(strings.Join(labels[i:], "."))
		if w == nil || w.Answers == nil || len(w.Answers) == 0 {
			break
		}
		if i == 1 {
			insertRecordData(records, w.Answers)
		} else {
			intersectRecordData(records, w.Answers)
		}
	}

	return records.Len() > 0
}

func intersectRecordData(set *stringset.Set, ans []*ExtractedAnswer) {
	records := stringset.New()
	defer records.Close()

	for _, a := range ans {
		records.Insert(strings.Trim(a.Data, "."))
	}
	set.Intersect(records)
}

func insertRecordData(set *stringset.Set, ans []*ExtractedAnswer) {
	records := stringset.New()
	defer records.Close()

	for _, a := range ans {
		records.Insert(strings.Trim(a.Data, "."))
	}
	set.Union(records)
}
