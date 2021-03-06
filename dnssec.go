// Copyright (c) 2013 Erik St. Martin, Brian Ketelsen. All rights reserved.
// Use of this source code is governed by The MIT License (MIT) that can be
// found in the LICENSE file.

package main

import (
	"crypto/sha1"
	"encoding/base32"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// Do DNSSEC NXDOMAIN with NSEC3 whitelies: rfc 7129, appendix B.
// The closest encloser will always be config.Domain and we
// will deny the wildcard for *.config.Domain. This allows
// use to pre-compute those records. We then only need to compute
// the NSEC3 that covers the qname.

var (
	cache    *sigCache = newCache()
	inflight *single   = new(single)
)

// ParseKeyFile read a DNSSEC keyfile as generated by dnssec-keygen or other
// utilities. It add ".key" for the public key and ".private" for the private key.
func ParseKeyFile(file string) (*dns.DNSKEY, dns.PrivateKey, error) {
	f, e := os.Open(file + ".key")
	if e != nil {
		return nil, nil, e
	}
	k, e := dns.ReadRR(f, file+".key")
	if e != nil {
		return nil, nil, e
	}
	f, e = os.Open(file + ".private")
	if e != nil {
		return nil, nil, e
	}
	p, e := k.(*dns.DNSKEY).ReadPrivateKey(f, file+".private")
	if e != nil {
		return nil, nil, e
	}
	return k.(*dns.DNSKEY), p, nil
}

// Denial creates (if needed) NSEC3 records that are included in the reply.
func (s *server) Denial(m *dns.Msg) {
	if m.Rcode == dns.RcodeNameError {
		// Deny Qname nsec3
		nsec3 := s.NewNSEC3NameError(m.Question[0].Name)
		m.Ns = append(m.Ns, nsec3)

		if nsec3.Hdr.Name != s.config.ClosestEncloser.Hdr.Name {
			m.Ns = append(m.Ns, s.config.ClosestEncloser)
		}
		if nsec3.Hdr.Name != s.config.DenyWildcard.Hdr.Name {
			m.Ns = append(m.Ns, s.config.DenyWildcard)
		}
	}
	if m.Rcode == dns.RcodeSuccess && len(m.Ns) == 1 {
		// NODATA
		if _, ok := m.Ns[0].(*dns.SOA); ok {
		m.Ns = append(m.Ns, s.NewNSEC3NoData(m.Question[0].Name))
		}
	}
}

// sign signs a message m, it takes care of negative or nodata responses as
// well by synthesising NSEC3 records. It will also cache the signatures, using
// a hash of the signed data as a key.
// We also fake the origin TTL in the signature, because we don't want to
// throw away signatures when services decide to have longer TTL. So we just
// set the origTTL to 60.
// TODO(miek): revisit origTTL
func (s *server) sign(m *dns.Msg, bufsize uint16) {
	now := time.Now().UTC()
	incep := uint32(now.Add(-3 * time.Hour).Unix())     // 2+1 hours, be sure to catch daylight saving time and such
	expir := uint32(now.Add(7 * 24 * time.Hour).Unix()) // sign for a week

	for _, r := range rrSets(m.Answer) {
		if r[0].Header().Rrtype == dns.TypeRRSIG {
			continue
		}
		if sig, err := s.signSet(r, now, incep, expir); err == nil {
			m.Answer = append(m.Answer, sig)
		}
	}
	for _, r := range rrSets(m.Ns) {
		if r[0].Header().Rrtype == dns.TypeRRSIG {
			continue
		}
		if sig, err := s.signSet(r, now, incep, expir); err == nil {
			m.Ns = append(m.Ns, sig)
		}
	}
	for _, r := range rrSets(m.Extra) {
		if r[0].Header().Rrtype == dns.TypeRRSIG {
			continue
		}
		if sig, err := s.signSet(r, now, incep, expir); err == nil {
			m.Extra = append(m.Extra, sig)
		}
	}
	if bufsize >= 512 || bufsize <= 4096 {
		m.Truncated = m.Len() > int(bufsize)
	}
	o := new(dns.OPT)
	o.Hdr.Name = "."
	o.Hdr.Rrtype = dns.TypeOPT
	o.SetDo()
	o.SetUDPSize(4096) // TODO(miek): echo client
	m.Extra = append(m.Extra, o)
	return
}

func (s *server) signSet(r []dns.RR, now time.Time, incep, expir uint32) (*dns.RRSIG, error) {
	key := cache.key(r)
	if sig := cache.search(key); sig != nil {
		// Is it still valid 24 hours from now?
		if sig.ValidityPeriod(now.Add(+24 * time.Hour)) {
			return sig, nil
		}
		cache.remove(key)
	}
	s.config.log.Infof("cache miss for %s type %d", r[0].Header().Name, r[0].Header().Rrtype)
	StatsDnssecCacheMiss.Inc(1)
	sig, err, shared := inflight.Do(key, func() (*dns.RRSIG, error) {
		sig1 := s.NewRRSIG(incep, expir)
		sig1.Header().Ttl = r[0].Header().Ttl
		if r[0].Header().Rrtype == dns.TypeTXT {
			sig1.OrigTtl = 0
		}
		e := sig1.Sign(s.config.PrivKey, r)
		if e != nil {
			s.config.log.Errorf("failed to sign: %s", e.Error())
		}
		return sig1, e
	})
	if err != nil {
		return nil, err
	}
	if !shared {
		cache.insert(key, sig)
	}
	return dns.Copy(sig).(*dns.RRSIG), nil
}

func (s *server) NewRRSIG(incep, expir uint32) *dns.RRSIG {
	sig := new(dns.RRSIG)
	sig.Hdr.Rrtype = dns.TypeRRSIG
	sig.Hdr.Ttl = s.config.Ttl
	sig.OrigTtl = s.config.Ttl
	sig.Algorithm = s.config.PubKey.Algorithm
	sig.KeyTag = s.config.KeyTag
	sig.Inception = incep
	sig.Expiration = expir
	sig.SignerName = s.config.PubKey.Hdr.Name
	return sig
}

func packBase32(s string) []byte {
	b32len := base32.HexEncoding.DecodedLen(len(s))
	buf := make([]byte, b32len)
	n, _ := base32.HexEncoding.Decode(buf, []byte(s))
	buf = buf[:n]
	return buf
}

func unpackBase32(b []byte) string {
	b32 := make([]byte, base32.HexEncoding.EncodedLen(len(b)))
	base32.HexEncoding.Encode(b32, b)
	return string(b32)
}

// NewNSEC3 returns the NSEC3 record needed to denial qname.
func (s *server) NewNSEC3NameError(qname string) *dns.NSEC3 {
	n := new(dns.NSEC3)
	n.Hdr.Class = dns.ClassINET
	n.Hdr.Rrtype = dns.TypeNSEC3
	n.Hdr.Ttl = s.config.MinTtl
	n.Hash = dns.SHA1
	n.Flags = 0
	n.Salt = ""
	n.TypeBitMap = []uint16{}

	covername := dns.HashName(qname, dns.SHA1, 0, "")

	buf := packBase32(covername)
	byteArith(buf, false) // one before
	n.Hdr.Name = strings.ToLower(unpackBase32(buf)) + "." + s.config.Domain
	byteArith(buf, true) // one next
	byteArith(buf, true) // and another one
	n.NextDomain = unpackBase32(buf)
	return n
}

// NewNSEC3 returns the NSEC3 record needed to denial the types
func (s *server) NewNSEC3NoData(qname string) *dns.NSEC3 {
	n := new(dns.NSEC3)
	n.Hdr.Class = dns.ClassINET
	n.Hdr.Rrtype = dns.TypeNSEC3
	n.Hdr.Ttl = s.config.MinTtl
	n.Hash = dns.SHA1
	n.Flags = 0
	n.Salt = ""
	n.TypeBitMap = []uint16{}

	n.Hdr.Name = dns.HashName(qname, dns.SHA1, 0, "")
	buf := packBase32(n.Hdr.Name)
	byteArith(buf, true) // one next
	n.NextDomain = unpackBase32(buf)

	n.Hdr.Name += "." + s.config.Domain
	return n
}

// newNSEC3CEandWildcard returns the NSEC3 for the closest encloser
// and the NSEC3 that denies that wildcard at that level.
func newNSEC3CEandWildcard(apex, ce string, ttl uint32) (*dns.NSEC3, *dns.NSEC3) {
	n1 := new(dns.NSEC3)
	n1.Hdr.Class = dns.ClassINET
	n1.Hdr.Rrtype = dns.TypeNSEC3
	n1.Hdr.Ttl = ttl
	n1.Hash = dns.SHA1
	n1.Flags = 0
	n1.Salt = ""
	//n.TypeBitMap = []uint16{dns.TypeA, dns.TypeNS, dns.TypeSOA, dns.TypeAAAA, dns.TypeRRSIG, dns.TypeDNSKEY}
	n1.TypeBitMap = []uint16{}
	n1.Hdr.Name = dns.HashName(ce, dns.SHA1, 0, "") + "." + apex
	buf := packBase32(n1.Hdr.Name)
	byteArith(buf, true) // one next
	n1.NextDomain = unpackBase32(buf)

	n2 := new(dns.NSEC3)
	n2.Hdr.Class = dns.ClassINET
	n2.Hdr.Rrtype = dns.TypeNSEC3
	n2.Hdr.Ttl = ttl
	n2.Hash = dns.SHA1
	n2.Flags = 0
	n2.Salt = ""

	buf = packBase32("*." + apex)
	byteArith(buf, false) // one before
	n2.Hdr.Name = strings.ToLower(unpackBase32(buf)) + "." + apex
	byteArith(buf, true) // one next
	byteArith(buf, true) // and another one
	n2.NextDomain = unpackBase32(buf)

	return n1, n2
}

// byteArith adds either 1 or -1 to b, there is no check for under- or overflow.
func byteArith(b []byte, x bool) {
	if x {
		for i := len(b) - 1; i >= 0; i-- {
			if b[i] == 255 {
				b[i] = 0
				continue
			}
			b[i] += 1
			return
		}
	}
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == 0 {
			b[i] = 255
			continue
		}
		b[i] -= 1
		return
	}
}

type rrset struct {
	qname string
	qtype uint16
}

func rrSets(rrs []dns.RR) map[rrset][]dns.RR {
	m := make(map[rrset][]dns.RR)
	for _, r := range rrs {
		if s, ok := m[rrset{r.Header().Name, r.Header().Rrtype}]; ok {
			s = append(s, r)
			m[rrset{r.Header().Name, r.Header().Rrtype}] = s
		} else {
			s := make([]dns.RR, 1, 3)
			s[0] = r
			m[rrset{r.Header().Name, r.Header().Rrtype}] = s
		}
	}
	if len(m) > 0 {
		return m
	}
	return nil
}

type sigCache struct {
	sync.RWMutex
	m map[string]*dns.RRSIG
}

func newCache() *sigCache {
	c := new(sigCache)
	c.m = make(map[string]*dns.RRSIG)
	return c
}

func (c *sigCache) remove(s string) {
	delete(c.m, s)
}

func (c *sigCache) insert(s string, r *dns.RRSIG) {
	c.Lock()
	defer c.Unlock()
	if _, ok := c.m[s]; !ok {
		c.m[s] = r
	}
}

func (c *sigCache) search(s string) *dns.RRSIG {
	c.RLock()
	defer c.RUnlock()
	if s, ok := c.m[s]; ok {
		// we want to return a copy here, because if we didn't the RRSIG
		// could be removed by another goroutine before the packet containing
		// this signature is send out.
		return dns.Copy(s).(*dns.RRSIG)
	}
	return nil
}

// key uses the name, type and rdata, which is serialized and then hashed as the
// key for the lookup
func (c *sigCache) key(rrs []dns.RR) string {
	h := sha1.New()
	i := []byte(rrs[0].Header().Name)
	i = append(i, packUint16(rrs[0].Header().Rrtype)...)
	for _, r := range rrs {
		switch t := r.(type) { // we only do a few type, serialize these manually
		case *dns.SOA:
			// We only fiddle with the serial so store that.
			i = append(i, packUint32(t.Serial)...)
		case *dns.SRV:
			i = append(i, packUint16(t.Priority)...)
			i = append(i, packUint16(t.Weight)...)
			i = append(i, packUint16(t.Weight)...)
			i = append(i, []byte(t.Target)...)
		case *dns.A:
			i = append(i, []byte(t.A)...)
		case *dns.AAAA:
			i = append(i, []byte(t.AAAA)...)
		case *dns.NSEC3:
			i = append(i, []byte(t.NextDomain)...)
			// Bitmap does not differentiate in SkyDNS.
		case *dns.DNSKEY:
		case *dns.NS:
		case *dns.TXT:
		}
	}
	return string(h.Sum(i))
}

func packUint16(i uint16) []byte { return []byte{byte(i >> 8), byte(i)} }
func packUint32(i uint32) []byte { return []byte{byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)} }

// Adapted from singleinflight.go from the original Go Code. Copyright 2013 The Go Authors.
type call struct {
	wg   sync.WaitGroup
	val  *dns.RRSIG
	err  error
	dups int
}

type single struct {
	sync.Mutex
	m map[string]*call
}

func (g *single) Do(key string, fn func() (*dns.RRSIG, error)) (*dns.RRSIG, error, bool) {
	g.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		c.dups++
		g.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := new(call)
	c.wg.Add(1)
	g.m[key] = c
	g.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.Lock()
	delete(g.m, key)
	g.Unlock()

	return c.val, c.err, c.dups > 0
}
