// Copyright 2015 Canonical Ltd.  This software is licensed under the
// GNU Lesser General Public License version 3 (see the file COPYING).

package gomaasapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

func getSubnetsEndpoint(version string) string {
	return fmt.Sprintf("/api/%s/subnets/", version)
}

// CreateSubnet is used to receive new subnets via the MAAS API
type CreateSubnet struct {
	DNSServers []string `json:"dns_servers"`
	Name       string   `json:"name"`
	Space      string   `json:"space"`
	GatewayIP  string   `json:"gateway_ip"`
	CIDR       string   `json:"cidr"`

	// VLAN this subnet belongs to. Currently ignored.
	// TODO: Defaults to the default VLAN
	// for the provided fabric or defaults to the default VLAN
	// in the default fabric.
	VLAN *uint `json:"vlan"`

	// Fabric for the subnet. Currently ignored.
	// TODO: Defaults to the fabric the provided
	// VLAN belongs to or defaults to the default fabric.
	Fabric *uint `json:"fabric"`

	// VID of the VLAN this subnet belongs to. Currently ignored.
	// TODO: Only used when vlan
	// is not provided. Picks the VLAN with this VID in the provided
	// fabric or the default fabric if one is not given.
	VID *uint `json:"vid"`

	// This is used for updates (PUT) and is ignored by create (POST)
	ID uint `json:"id"`
}

// Subnet is the MAAS API subnet representation
type Subnet struct {
	DNSServers []string `json:"dns_servers"`
	Name       string   `json:"name"`
	Space      string   `json:"string"`
	VLAN       VLAN     `json:"vlan"`
	GatewayIP  string   `json:"gateway_ip"`
	CIDR       string   `json:"cidr"`

	ResourceURI      string `json:"resource_uri"`
	ID               uint   `json:"id"`
	InUseIPAddresses []IP   `json:"-"`
}

// subnetsHandler handles requests for '/api/<version>/subnets/'.
func subnetsHandler(server *TestServer, w http.ResponseWriter, r *http.Request) {
	var err error
	values, err := url.ParseQuery(r.URL.RawQuery)
	checkError(err)
	op := values.Get("op")
	includeRangesString := strings.ToLower(values.Get("include_ranges"))
	subnetsURLRE := regexp.MustCompile(`/subnets/(.+?)/`)
	subnetsURLMatch := subnetsURLRE.FindStringSubmatch(r.URL.Path)
	subnetsURL := getSubnetsEndpoint(server.version)

	var ID uint
	var gotID bool
	if subnetsURLMatch != nil {
		ID, err = NameOrIDToID(subnetsURLMatch[1], server.subnetNameToID, 1, uint(len(server.subnets)))

		if err != nil {
			http.NotFoundHandler().ServeHTTP(w, r)
			return
		}

		gotID = true
	}

	var includeRanges bool
	switch includeRangesString {
	case "true", "yes", "1":
		includeRanges = true
	}

	switch r.Method {
	case "GET":
		w.Header().Set("Content-Type", "application/vnd.api+json")
		if len(server.subnets) == 0 {
			// Until a subnet is registered, behave as if the endpoint
			// does not exist. This way we can simulate older MAAS
			// servers that do not support subnets.
			http.NotFoundHandler().ServeHTTP(w, r)
			return
		}

		if r.URL.Path == subnetsURL {
			var subnets []Subnet
			for i := uint(1); i < server.nextSubnet; i++ {
				s, ok := server.subnets[i]
				if ok {
					subnets = append(subnets, s)
				}
			}
			err = json.NewEncoder(w).Encode(subnets)
		} else if gotID == false {
			w.WriteHeader(http.StatusBadRequest)
		} else {
			switch op {
			case "unreserved_ip_ranges":
				err = json.NewEncoder(w).Encode(
					server.subnetUnreservedIPRanges(server.subnets[ID]))
			case "reserved_ip_ranges":
				err = json.NewEncoder(w).Encode(
					server.subnetReservedIPRanges(server.subnets[ID]))
			case "statistics":
				err = json.NewEncoder(w).Encode(
					server.subnetStatistics(server.subnets[ID], includeRanges))
			default:
				err = json.NewEncoder(w).Encode(server.subnets[ID])
			}
		}
		checkError(err)
	case "POST":
		server.NewSubnet(r.Body)
	case "PUT":
		server.UpdateSubnet(r.Body)
	case "DELETE":
		delete(server.subnets, ID)
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

type addressList []IP

func (a addressList) Len() int           { return len(a) }
func (a addressList) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a addressList) Less(i, j int) bool { return a[i].UInt64() < a[j].UInt64() }

// AddressRange is used to generate reserved IP address range lists
type AddressRange struct {
	Start        string `json:"start"`
	startUint    uint64
	End          string `json:"end"`
	endUint      uint64
	Purpose      []string `json:"purpose,omitempty"`
	NumAddresses uint     `json:"num_addresses"`
}

func (server *TestServer) subnetUnreservedIPRanges(subnet Subnet) []AddressRange {
	// Make a sorted copy of subnet.InUseIPAddresses
	ipAddresses := make([]IP, len(subnet.InUseIPAddresses))
	copy(ipAddresses, subnet.InUseIPAddresses)
	sort.Sort(addressList(ipAddresses))

	// We need the first and last address in the subnet
	var ranges []AddressRange
	var i AddressRange
	var startIP, endIP, lastUsableIP IP

	_, ipNet, err := net.ParseCIDR(subnet.CIDR)
	checkError(err)
	startIP = IPFromNetIP(ipNet.IP)
	// Start with the lowest usable address in the range, which is 1 above
	// what net.ParseCIDR will give back.
	startIP.SetUInt64(startIP.UInt64() + 1)

	ones, bits := ipNet.Mask.Size()
	set := ^((^uint64(0)) << uint(bits-ones))

	// The last usable address is one below the broadcast address, which is
	// what you get by bitwise ORing 'set' with any IP address in the subnet.
	lastUsableIP.SetUInt64((startIP.UInt64() | set) - 1)

	for _, endIP = range ipAddresses {
		end := endIP.UInt64()

		if endIP.UInt64() == startIP.UInt64() {
			if endIP.UInt64() != lastUsableIP.UInt64() {
				startIP.SetUInt64(end + 1)
			}
			continue
		}

		if end == lastUsableIP.UInt64() {
			continue
		}

		endIP.SetUInt64(end - 1)
		i.Start, i.End = startIP.String(), endIP.String()
		i.startUint, i.endUint = startIP.UInt64(), endIP.UInt64()
		i.NumAddresses = uint(1 + endIP.UInt64() - startIP.UInt64())
		ranges = append(ranges, i)
		startIP.SetUInt64(end + 1)
	}

	if startIP.UInt64() != lastUsableIP.UInt64() {
		i.Start, i.End = startIP.String(), lastUsableIP.String()
		i.startUint, i.endUint = startIP.UInt64(), lastUsableIP.UInt64()
		i.NumAddresses = uint(1 + lastUsableIP.UInt64() - startIP.UInt64())
		ranges = append(ranges, i)
	}

	return ranges
}

func (server *TestServer) subnetReservedIPRanges(subnet Subnet) []AddressRange {
	// Make a sorted copy of subnet.InUseIPAddresses
	ipAddresses := make([]IP, len(subnet.InUseIPAddresses))
	copy(ipAddresses, subnet.InUseIPAddresses)
	sort.Sort(addressList(ipAddresses))

	var ranges []AddressRange
	var i AddressRange
	var startIP, thisIP IP
	startIP = ipAddresses[0]
	lastIP := ipAddresses[0].UInt64()

	for _, thisIP = range ipAddresses {
		ip := thisIP.UInt64()
		if ip != lastIP && ip != lastIP+1 {
			thisIP.SetUInt64(lastIP)
			i.Start, i.End = startIP.String(), thisIP.String()
			i.startUint, i.endUint = startIP.UInt64(), thisIP.UInt64()
			i.NumAddresses = uint(1 + thisIP.UInt64() - startIP.UInt64())
			ranges = append(ranges, i)
			startIP.SetUInt64(ip)
		}
		lastIP = ip
	}
	if ranges[len(ranges)-1].endUint != lastIP {
		thisIP.SetUInt64(lastIP)
		i.Start, i.End = startIP.String(), thisIP.String()
		i.startUint, i.endUint = startIP.UInt64(), thisIP.UInt64()
		i.NumAddresses = uint(1 + thisIP.UInt64() - startIP.UInt64())
		ranges = append(ranges, i)
	}

	return ranges
}

// SubnetStats holds statistics about a subnet
type SubnetStats struct {
	NumAvailable     uint           `json:"num_available"`
	LargestAvailable uint           `json:"largest_available"`
	NumUnavailable   uint           `json:"num_unavailable"`
	TotalAddresses   uint           `json:"total_addresses"`
	Usage            float32        `json:"usage"`
	UsageString      string         `json:"usage_string"`
	Ranges           []AddressRange `json:"ranges"`
}

func (server *TestServer) subnetStatistics(subnet Subnet, includeRanges bool) SubnetStats {
	var stats SubnetStats
	_, ipNet, err := net.ParseCIDR(subnet.CIDR)
	checkError(err)

	ones, bits := ipNet.Mask.Size()
	stats.TotalAddresses = (1 << uint(bits-ones)) - 2
	stats.NumUnavailable = uint(len(subnet.InUseIPAddresses))
	stats.NumAvailable = stats.TotalAddresses - stats.NumUnavailable
	stats.Usage = float32(stats.NumUnavailable) / float32(stats.TotalAddresses)
	stats.UsageString = fmt.Sprintf("%0.1f%%", stats.Usage*100)

	// Calculate stats.LargestAvailable - the largest contiguous block of IP addresses available
	reserved := server.subnetUnreservedIPRanges(subnet)
	for _, addressRange := range reserved {
		if addressRange.NumAddresses > stats.LargestAvailable {
			stats.LargestAvailable = addressRange.NumAddresses
		}
	}

	if includeRanges {
		stats.Ranges = reserved
	}

	return stats
}

func decodePostedSubnet(subnetJSON io.Reader) CreateSubnet {
	var postedSubnet CreateSubnet
	decoder := json.NewDecoder(subnetJSON)
	err := decoder.Decode(&postedSubnet)
	checkError(err)
	return postedSubnet
}

// UpdateSubnet creates a subnet in the test server
func (server *TestServer) UpdateSubnet(subnetJSON io.Reader) Subnet {
	postedSubnet := decodePostedSubnet(subnetJSON)
	updatedSubnet := subnetFromCreateSubnet(postedSubnet)
	server.subnets[updatedSubnet.ID] = updatedSubnet
	return updatedSubnet
}

// NewSubnet creates a subnet in the test server
func (server *TestServer) NewSubnet(subnetJSON io.Reader) *Subnet {
	postedSubnet := decodePostedSubnet(subnetJSON)
	newSubnet := subnetFromCreateSubnet(postedSubnet)
	newSubnet.ID = server.nextSubnet
	server.subnets[server.nextSubnet] = newSubnet
	server.subnetNameToID[newSubnet.Name] = newSubnet.ID

	server.nextSubnet++
	return &newSubnet
}

// NodeNetworkInterface represents a network interface attached to a node
type NodeNetworkInterface struct {
	Name  string        `json:"name"`
	Links []NetworkLink `json:"links"`
}

// Node represents a node
type Node struct {
	SystemID   string                 `json:"system_id"`
	Interfaces []NodeNetworkInterface `json:"interface_set"`
}

// NetworkLink represents a MAAS network link
type NetworkLink struct {
	ID     uint    `json:"id"`
	Mode   string  `json:"mode"`
	Subnet *Subnet `json:"subnet"`
}

// SetNodeNetworkLink recordds that the given node + interface are in subnet
func (server *TestServer) SetNodeNetworkLink(node Node, nodeNetworkInterface NodeNetworkInterface) {
	for i, ni := range server.nodeMetadata[node.SystemID].Interfaces {
		if ni.Name == nodeNetworkInterface.Name {
			server.nodeMetadata[node.SystemID].Interfaces[i] = nodeNetworkInterface
			return
		}
	}
	n := server.nodeMetadata[node.SystemID]
	n.Interfaces = append(n.Interfaces, nodeNetworkInterface)
	server.nodeMetadata[node.SystemID] = n
}

// subnetFromCreateSubnet creates a subnet in the test server
func subnetFromCreateSubnet(postedSubnet CreateSubnet) Subnet {
	var newSubnet Subnet
	newSubnet.DNSServers = postedSubnet.DNSServers
	newSubnet.Name = postedSubnet.Name
	newSubnet.Space = postedSubnet.Space
	//TODO: newSubnet.VLAN = server.postedSubnetVLAN
	newSubnet.GatewayIP = postedSubnet.GatewayIP
	newSubnet.CIDR = postedSubnet.CIDR
	newSubnet.ID = postedSubnet.ID
	return newSubnet
}
