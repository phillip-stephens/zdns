/*
 * ZDNS Copyright 2024 Regents of the University of Michigan
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not
 * use this file except in compliance with the License. You may obtain a copy
 * of the License at http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
 * implied. See the License for the specific language governing
 * permissions and limitations under the License.
 */
package mxlookup

import (
	"strings"
	"sync"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/zmap/dns"

	"github.com/zmap/zdns/src/cli"
	"github.com/zmap/zdns/src/internal/cachehash"
	"github.com/zmap/zdns/src/zdns"
)

type CachedAddresses struct {
	IPv4Addresses []string
	IPv6Addresses []string
}

func init() {
	mx := new(MXLookupModule)
	cli.RegisterLookupModule("MXLOOKUP", mx)
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

type MXLookupModule struct {
	IPv4Lookup  bool `long:"ipv4-lookup" description:"perform A lookups for each MX server"`
	IPv6Lookup  bool `long:"ipv6-lookup" description:"perform AAAA record lookups for each MX server"`
	MXCacheSize int  `long:"mx-cache-size" default:"1000" description:"number of records to store in MX -> A/AAAA cache"`
	CacheHash   *cachehash.CacheHash
	CHmu        sync.Mutex
	cli.BasicLookupModule
}

// CLIInit initializes the MXLookupModule with the given parameters, used to call MXLookup from the command line
func (mxMod *MXLookupModule) CLIInit(gc *cli.CLIConf, rc *zdns.ResolverConfig) error {
	if !mxMod.IPv4Lookup && !mxMod.IPv6Lookup {
		// need to use one of the two
		mxMod.IPv4Lookup = true
	}
	mxMod.Init()
	if err := mxMod.BasicLookupModule.CLIInit(gc, rc); err != nil {
		return errors.Wrap(err, "failed to initialize BasicLookupModule")
	}
	return nil
}

// Init initializes the MXLookupModule with the given parameters, used to call MXLookup programmatically
func (mxMod *MXLookupModule) Init() {
	if !mxMod.IPv4Lookup && !mxMod.IPv6Lookup {
		log.Fatal("At least one of ipv4-lookup or ipv6-lookup must be true")
	}
	if mxMod.MXCacheSize <= 0 {
		log.Fatal("mxCacheSize must be greater than 0, got ", mxMod.MXCacheSize)
	}
	mxMod.CacheHash = new(cachehash.CacheHash)
	mxMod.CacheHash.Init(mxMod.MXCacheSize)
}

func (mxMod *MXLookupModule) lookupIPs(r *zdns.Resolver, name, nameServer string, ipMode zdns.IPVersionMode) (CachedAddresses, zdns.Trace) {
	mxMod.CHmu.Lock()
	// TODO - Phillip this comment V is present in the original code and has been there since 2017 IIRC, so ask Zakir what to do
	// XXX this should be changed to a miekglookup
	res, found := mxMod.CacheHash.Get(name)
	mxMod.CHmu.Unlock()
	if found {
		return res.(CachedAddresses), zdns.Trace{}
	}
	retv := CachedAddresses{}
	result, trace, status, _ := r.DoTargetedLookup(name, nameServer, mxMod.IsIterative, mxMod.IPv4Lookup, mxMod.IPv6Lookup)
	if status == zdns.StatusNoError && result != nil {
		retv.IPv4Addresses = result.IPv4Addresses
		retv.IPv6Addresses = result.IPv6Addresses
	}
	mxMod.CHmu.Lock()
	mxMod.CacheHash.Upsert(name, retv)
	mxMod.CHmu.Unlock()
	return retv, trace
}

func (mxMod *MXLookupModule) Lookup(r *zdns.Resolver, lookupName, nameServer string) (interface{}, zdns.Trace, zdns.Status, error) {
	ipMode := zdns.GetIPVersionMode(mxMod.IPv4Lookup, mxMod.IPv6Lookup)
	retv := MXResult{Servers: []MXRecord{}}
	var res *zdns.SingleQueryResult
	var trace zdns.Trace
	var status zdns.Status
	var err error
	if mxMod.BasicLookupModule.IsIterative {
		res, trace, status, err = r.IterativeLookup(&zdns.Question{Name: lookupName, Type: dns.TypeMX, Class: dns.ClassINET})
	} else {
		res, trace, status, err = r.ExternalLookup(&zdns.Question{Name: lookupName, Type: dns.TypeMX, Class: dns.ClassINET}, nameServer)
	}
	if status != zdns.StatusNoError || err != nil {
		return nil, trace, status, err
	}

	for _, ans := range res.Answers {
		if mxAns, ok := ans.(zdns.PrefAnswer); ok {
			lookupName = strings.TrimSuffix(mxAns.Answer.Answer, ".")
			rec := MXRecord{TTL: mxAns.TTL, Type: mxAns.Type, Class: mxAns.Class, Name: lookupName, Preference: mxAns.Preference}
			ips, secondTrace := mxMod.lookupIPs(r, lookupName, nameServer, ipMode)
			rec.IPv4Addresses = ips.IPv4Addresses
			rec.IPv6Addresses = ips.IPv6Addresses
			retv.Servers = append(retv.Servers, rec)
			trace = append(trace, secondTrace...)
		}
	}
	return &retv, trace, zdns.StatusNoError, nil
}

// Help returns the module's help string
func (mxMod *MXLookupModule) Help() string {
	return ""
}

func (mxMod *MXLookupModule) Description() string {
	return "MXLOOKUP will additionally do an A lookup for the IP addresses that correspond with an exchange record."
}
