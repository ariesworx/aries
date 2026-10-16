package server

import (
	clist "container/list"
	"time"
)

// signInHolds is what the authorization server holds between requests:
// the sign-ins sent to the provider and not yet back, by the server's
// state there, and the codes aries has not spent, by their hash. Anyone
// may start a sign-in, so both are bounded: most in all, past which the
// oldest is let go of, so that requests never meant to finish cannot
// refuse a person's; and perAddr from one client address, past which
// the address is refused. perAddr is zero behind a proxy, where every
// request is the proxy's.
//
// Each kind is a list in the order it was added, each with the time it
// was added, taken under authServer.mu. Every entry of a kind lives as
// long, and the store's time does not go backward (rule 19), so that is
// the order they lapse in too: what lapsed, and the oldest, are at the
// front, and nothing is found by a scan. The caller holds
// authServer.mu.
type signInHolds struct {
	most, perAddr int
	pending       holdQueue[*pendingSignIn]
	codes         holdQueue[*issuedCode]
	byAddr        map[string]int
}

func newSignInHolds(most, perAddr int) *signInHolds {
	return &signInHolds{most: most, perAddr: perAddr,
		pending: holdQueue[*pendingSignIn]{life: signInWait}, codes: holdQueue[*issuedCode]{life: codeLife}, byAddr: map[string]int{}}
}

// addPending holds ps, started from addr at now, under key. It reports
// false, and holds nothing, when addr holds perAddr already, and
// whether it let go of the oldest to make room.
func (s *signInHolds) addPending(now time.Time, addr, key string, ps *pendingSignIn) (held, evicted bool) {
	s.expire(now)
	if s.perAddr > 0 && s.byAddr[addr] >= s.perAddr {
		return false, false
	}
	evicted = s.room()
	s.pending.push(&holdEntry[*pendingSignIn]{key: key, addr: addr, at: now, v: ps})
	s.byAddr[addr]++
	return true, evicted
}

// addCode holds ic under key, for the sign-in addr started, and reports
// whether it let go of the oldest to make room. A code is never refused
// for its address: it takes the place of the sign-in that takePending
// let go of when the provider sent the browser back. So an address holds
// perAddr at most, and for a moment one more for each of its sign-ins
// the provider is sending back.
func (s *signInHolds) addCode(now time.Time, addr, key string, ic *issuedCode) (evicted bool) {
	s.expire(now)
	evicted = s.room()
	s.codes.push(&holdEntry[*issuedCode]{key: key, addr: addr, at: now, v: ic})
	s.byAddr[addr]++
	return evicted
}

// takePending lets go of the sign-in held under key, and returns it with
// the address that started it.
func (s *signInHolds) takePending(key string) (*pendingSignIn, string) {
	e := s.pending.take(key)
	if e == nil {
		return nil, ""
	}
	s.release(e.addr)
	return e.v, e.addr
}

// takeCode lets go of the code held under key, and returns it.
func (s *signInHolds) takeCode(key string) *issuedCode {
	e := s.codes.take(key)
	if e == nil {
		return nil
	}
	s.release(e.addr)
	return e.v
}

// expire lets go of what lapsed by now.
func (s *signInHolds) expire(now time.Time) {
	for e := s.pending.front(); e != nil && s.pending.lapsed(e, now); e = s.pending.front() {
		s.takePending(e.key)
	}
	for e := s.codes.front(); e != nil && s.codes.lapsed(e, now); e = s.codes.front() {
		s.takeCode(e.key)
	}
}

// room lets go of the oldest sign-in or code while most are held, and
// reports whether it did.
func (s *signInHolds) room() bool {
	evicted := false
	for s.pending.len()+s.codes.len() >= s.most {
		p, c := s.pending.front(), s.codes.front()
		if c == nil || p != nil && p.at.Before(c.at) {
			s.takePending(p.key)
		} else {
			s.takeCode(c.key)
		}
		evicted = true
	}
	return evicted
}

func (s *signInHolds) release(addr string) {
	if s.byAddr[addr]--; s.byAddr[addr] <= 0 {
		delete(s.byAddr, addr)
	}
}

// holdQueue is one kind of what signInHolds holds: a list, oldest
// first, and an index of it by key. Every entry lives life from its at.
type holdQueue[V any] struct {
	life time.Duration
	l    clist.List
	m    map[string]*clist.Element
}

// holdEntry is one thing a holdQueue holds, with the address that
// started it.
type holdEntry[V any] struct {
	key, addr string
	at        time.Time
	v         V
}

func (q *holdQueue[V]) push(e *holdEntry[V]) {
	if q.m == nil {
		q.m = map[string]*clist.Element{}
	}
	q.m[e.key] = q.l.PushBack(e)
}

func (q *holdQueue[V]) take(key string) *holdEntry[V] {
	el := q.m[key]
	if el == nil {
		return nil
	}
	delete(q.m, key)
	return q.l.Remove(el).(*holdEntry[V])
}

func (q *holdQueue[V]) front() *holdEntry[V] {
	if el := q.l.Front(); el != nil {
		return el.Value.(*holdEntry[V])
	}
	return nil
}

func (q *holdQueue[V]) lapsed(e *holdEntry[V], now time.Time) bool {
	return !now.Before(e.at.Add(q.life))
}

func (q *holdQueue[V]) len() int { return q.l.Len() }
