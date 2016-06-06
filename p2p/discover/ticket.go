// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package discover implements the Node Discovery Protocol.
//
// The Node Discovery protocol provides a way to find RLPx nodes that
// can be connected to. It uses a Kademlia-like protocol to maintain a
// distributed database of the IDs and endpoints of all listening
// nodes.
package discover

import (
	"encoding/binary"
	"math/rand"
	"time"

	"github.com/aristanetworks/goarista/atime"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	ticketGroupTime = uint64(time.Minute)
	timeWindow      = 30
	keepTicketConst = uint64(time.Minute) * 20
	keepTicketExp   = uint64(time.Minute) * 20
	maxRadius       = 0xffffffffffffffff
	minRadAverage   = 1024
)

type ticket struct {
	topics  []Topic
	regTime []uint64 // Per-topic local absolute time when the ticket can be used.

	// The serial number that was issued by the server.
	serial uint32
	// Used by registrar, tracks absolute time when the ticket was created.
	issueTime uint64

	// Fields used only by registrants
	node   *Node  // the registrar node that signed this ticket
	refCnt int    // tracks number of topics that will be registered using this ticket
	pong   []byte // encoded pong packet signed by the registrar
}

func pongToTicket(localTime uint64, topics []Topic, node *Node, p *ingressPacket) *ticket {
	wps := p.data.(*pong).WaitPeriods
	t := &ticket{
		issueTime: localTime,
		node:      node,
		topics:    topics,
		pong:      p.rawData,
		regTime:   make([]uint64, len(wps)),
	}
	// Convert wait periods to local absolute time.
	for i, wp := range wps {
		t.regTime[i] = localTime + uint64(wp)*uint64(time.Second)
	}
	return t
}

func ticketToPong(t *ticket, pong *pong) {
	pong.Expiration = uint64(time.Duration(t.issueTime) / time.Second)
	pong.TopicHash = rlpHash(t.topics)
	pong.TicketSerial = t.serial
	pong.WaitPeriods = make([]uint32, len(t.regTime))
	for i, regTime := range t.regTime {
		pong.WaitPeriods[i] = uint32(time.Duration(regTime) / time.Second)
	}
}

type ticketStore struct {
	topics               map[Topic]*topicTickets
	nodes                map[NodeID]*ticket
	lastGroupFetched     uint64
	minRadSum            float64
	minRadCnt, minRadius uint64
}

func newTicketStore() *ticketStore {
	return &ticketStore{
		topics: make(map[Topic]*topicTickets),
		nodes:  make(map[NodeID]*ticket),
	}
}

// addTopic starts tracking a topic. If register is true,
// the local node will register the topic and tickets will be collected.
// It can be called even
func (s *ticketStore) addTopic(t Topic, register bool) {
	if s.topics[t] == nil {
		s.topics[t] = newtopicTickets(t, register)
	}
}

// nextTarget returns the target of the next lookup for registration
// or topic search.
func (s *ticketStore) nextTarget(t Topic) common.Hash {
	return s.topics[t].nextTarget()
}

// ticketsInWindow returns the number of tickets in the registration window.
func (s *ticketStore) ticketsInWindow(t Topic) int {
	now := atime.NanoTime()
	ltGroup := now / ticketGroupTime
	sum := 0
	for g := ltGroup; g < ltGroup+timeWindow; g++ {
		sum += len(s.topics[t].time[g])
	}
	return sum
}

// nextRegisterableTicket returns the next ticket that can be used
// to register.
//
// If the returned wait time is zero the ticket can be used. For a non-zero
// wait time, the caller should requery the next ticket later.
//
// A ticket can be returned more than once with zero wait time in case
// the ticket contains multiple topics.
func (s *ticketStore) nextRegisterableTicket() (t *ticket, wait time.Duration) {
	return nil, 0
}

func (s *ticketStore) add(localTime uint64, t *ticket) {
	if s.nodes[t.node.ID] != nil {
		return
	}

	if s.lastGroupFetched == 0 {
		s.lastGroupFetched = localTime / ticketGroupTime
	}

	for i, topic := range t.topics {
		if tt, ok := s.topics[topic]; ok && tt.isInRadius(t) {
			tt.adjust(localTime, ticketRef{t, i}, s.minRadius)

			if tt.converged {
				wait := t.regTime[i] - localTime
				rnd := rand.ExpFloat64()
				if rnd > 10 {
					rnd = 10
				}
				if tt.time != nil && float64(wait) < float64(keepTicketConst)+float64(keepTicketExp)*rnd {
					// use the ticket to register this topic
					tgroup := t.regTime[i] / ticketGroupTime
					tt.time[tgroup] = append(tt.time[tgroup], ticketRef{t, i})
					t.refCnt++
				}
			}
		}
	}

	if t.refCnt > 0 {
		s.nodes[t.node.ID] = t
	}
}

func (s *ticketStore) fetch(localTime uint64) (res []*ticket) {
	ltGroup := localTime / ticketGroupTime
	for _, tt := range s.topics {
		for g := s.lastGroupFetched; g <= ltGroup; g++ {
			list := tt.time[g]
			i := 0
			for i < len(list) {
				t := list[i]
				if t.t.regTime[t.idx] <= localTime {
					list = append(list[:i], list[i+1:]...)
					res = append(res, t.t)
					t.t.refCnt--
					if t.t.refCnt == 0 {
						delete(s.nodes, t.t.node.ID)
					}
				} else {
					i++
				}
			}
			if g != ltGroup {
				delete(tt.time, g)
			}
		}
	}
	s.lastGroupFetched = ltGroup
	return res
}

func (s *ticketStore) getNodeTicket(id NodeID) *ticket {
	return s.nodes[id]
}

func (s *ticketStore) adjustMinRadius(target, found NodeID) {
	tp := binary.BigEndian.Uint64(target[0:8])
	fp := binary.BigEndian.Uint64(found[0:8])
	dist := tp ^ fp
	mrAdjust := float64(dist) * 16
	if mrAdjust > maxRadius/2 {
		mrAdjust = maxRadius / 2
	}

	if s.minRadCnt < minRadAverage {
		s.minRadCnt++
	} else {
		s.minRadSum -= s.minRadSum / minRadAverage
	}
	s.minRadSum += mrAdjust
	s.minRadius = uint64(s.minRadSum / float64(s.minRadCnt))
}

type ticketRef struct {
	t   *ticket
	idx int
}

type topicTickets struct {
	topic           Topic
	topicHashPrefix uint64
	radius          uint64
	filteredRadius  float64 // only for convergence detection
	converged       bool

	// Contains buckets (for each absolute minute) of tickets
	// that can be used in that minute.
	// This is only set if the topic is being registered.
	time map[uint64][]ticketRef
}

const targetWaitTime = uint64(time.Minute) * 10

func newtopicTickets(t Topic, register bool) *topicTickets {
	topicHash := crypto.Keccak256Hash([]byte(t))
	topicHashPrefix := binary.BigEndian.Uint64(topicHash[0:8])

	tt := &topicTickets{
		topic:           t,
		topicHashPrefix: topicHashPrefix,
		radius:          maxRadius,
		filteredRadius:  float64(maxRadius),
		converged:       false,
	}
	if register {
		tt.time = make(map[uint64][]ticketRef)
	}
	return tt
}

func (r *topicTickets) isInRadius(t *ticket) bool {
	nodePrefix := binary.BigEndian.Uint64(t.node.ID[0:8])
	dist := nodePrefix ^ r.topicHashPrefix
	return dist < r.radius
}

func (r *topicTickets) nextTarget() common.Hash {
	rnd := uint64(rand.Int63n(int64(r.radius/2))) * 2
	prefix := r.topicHashPrefix ^ rnd
	var target common.Hash
	binary.BigEndian.PutUint64(target[0:8], prefix)
	return target
}

func (r *topicTickets) adjust(localTime uint64, t ticketRef, minRadius uint64) {
	wait := t.t.regTime[t.idx] - localTime
	adjust := (float64(wait)/float64(targetWaitTime) - 1) * 2
	if adjust > 1 {
		adjust = 1
	}
	if adjust < -1 {
		adjust = -1
	}
	if r.converged {
		adjust *= 0.01
	} else {
		adjust *= 0.1
	}

	radius := float64(r.radius) * (1 + adjust)
	if radius > float64(maxRadius) {
		r.radius = maxRadius
		radius = float64(r.radius)
	} else {
		r.radius = uint64(radius)
		if r.radius < minRadius {
			r.radius = minRadius
		}
	}

	if !r.converged {
		if radius >= r.filteredRadius {
			r.converged = true
		} else {
			r.filteredRadius += (radius - r.filteredRadius) * 0.05
		}
	}
}
