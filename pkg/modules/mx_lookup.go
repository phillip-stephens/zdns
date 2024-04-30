package modules

import (
	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/zmap/dns"
	"github.com/zmap/zdns/cachehash"
	"github.com/zmap/zdns/pkg/zdns"
	"strings"
	"sync"
)

type CachedAddresses struct {
	IPv4Addresses []string
	IPv6Addresses []string
}

type MXRecord struct {
	Name          string   `json:"name" groups:"short,normal,long,trace"`
	Type          string   `json:"type" groups:"short,normal,long,trace"`
	Class         string   `json:"class" groups:"normal,long,trace"`
	Preference    uint16   `json:"preference" groups:"short,normal,long,trace"`
	IPv4Addresses []string `json:"ipv4_addresses,omitempty" groups:"short,normal,long,trace"`
	IPv6Addresses []string `json:"ipv6_addresses,omitempty" groups:"short,normal,long,trace"`
	TTL           uint32   `json:"ttl" groups:"ttl,normal,long,trace"`
}

type MXResult struct {
	Servers []MXRecord `json:"exchanges" groups:"short,normal,long,trace"`
}

type MXLookup struct {
	IPv4Lookup  bool
	IPv6Lookup  bool
	MXCacheSize int
	CacheHash   *cachehash.CacheHash
	CHmu        sync.Mutex
}

func Initialize(f *pflag.FlagSet) *MXLookup {

	ipv4Lookup, err := f.GetBool("ipv4-lookup")
	if err != nil {
		panic(err)
	}
	ipv6Lookup, err := f.GetBool("ipv6-lookup")
	if err != nil {
		panic(err)
	}
	mxCacheSize, err := f.GetInt("mx-cache-size")
	if err != nil {
		panic(err)
	}

	mxLookup := new(MXLookup)
	mxLookup.IPv4Lookup = ipv4Lookup
	mxLookup.IPv6Lookup = ipv6Lookup
	mxLookup.MXCacheSize = mxCacheSize
	mxLookup.CacheHash = new(cachehash.CacheHash)
	mxLookup.CacheHash.Init(mxCacheSize)
	return mxLookup
}

func (mxLookup *MXLookup) LookupIPs(r *zdns.Resolver, name, nameServer string, lookupIpv4 bool, lookupIpv6 bool) (CachedAddresses, zdns.Trace) {
	if mxLookup == nil {
		log.Fatal("mxLookup is not initialized")
	}
	mxLookup.CHmu.Lock()
	// XXX this should be changed to a miekglookup
	res, found := mxLookup.CacheHash.Get(name)
	mxLookup.CHmu.Unlock()
	if found {
		return res.(CachedAddresses), zdns.Trace{}
	}
	retv := CachedAddresses{}
	result, trace, status, _ := DoTargetedLookup(r, name, nameServer, lookupIpv4, lookupIpv6)
	if status == zdns.STATUS_NOERROR && result != nil {
		retv.IPv4Addresses = result.IPv4Addresses
		retv.IPv6Addresses = result.IPv6Addresses
	}

	mxLookup.CHmu.Lock()
	mxLookup.CacheHash.Add(name, retv)
	mxLookup.CHmu.Unlock()
	return retv, trace
}

func (mxLookup *MXLookup) DoLookup(r *zdns.Resolver, name, nameServer string) (*MXResult, zdns.Trace, zdns.Status, error) {
	retv := MXResult{Servers: []MXRecord{}}
	res, trace, status, err := r.ExternalLookup(&zdns.Question{Name: name, Type: dns.TypeMX, Class: dns.ClassINET}, nameServer)
	if status != zdns.STATUS_NOERROR || err != nil {
		return nil, trace, status, err
	}

	lookupIpv4 := mxLookup.IPv4Lookup || !mxLookup.IPv6Lookup
	lookupIpv6 := mxLookup.IPv6Lookup
	for _, ans := range res.Answers {
		if mxAns, ok := ans.(zdns.PrefAnswer); ok {
			name = strings.TrimSuffix(mxAns.Answer.Answer, ".")
			rec := MXRecord{TTL: mxAns.Ttl, Type: mxAns.Type, Class: mxAns.Class, Name: name, Preference: mxAns.Preference}
			ips, secondTrace := mxLookup.LookupIPs(r, name, nameServer, lookupIpv4, lookupIpv6)
			rec.IPv4Addresses = ips.IPv4Addresses
			rec.IPv6Addresses = ips.IPv6Addresses
			retv.Servers = append(retv.Servers, rec)
			trace = append(trace, secondTrace...)
		}
	}
	return &retv, trace, zdns.STATUS_NOERROR, nil
}

// TODO Phillip pull thesse CLI flags over
//func (s *GlobalLookupFactory) SetFlags(f *pflag.FlagSet) {
//	// If there's an error, panic is appropriate since we should at least be getting the default here.
//	var err error
//	s.IPv4Lookup, err = f.GetBool("ipv4-lookup")
//	if err != nil {
//		panic(err)
//	}
//	s.IPv6Lookup, err = f.GetBool("ipv6-lookup")
//	if err != nil {
//		panic(err)
//	}
//	s.MXCacheSize, err = f.GetInt("mx-cache-size")
//	if err != nil {
//		panic(err)
//	}
//}
