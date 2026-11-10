package test

import (
	"encoding/binary"
	"net"
	"testing"
)

// GeoIPDatabase returns a tiny database in MaxMind's format (the GeoLite2-
// Country layout, IPv4 only) that puts every address in network, an IPv4
// /24 such as "81.2.69.0", in the country iso ("GB"). Other addresses have no
// country.
func GeoIPDatabase(t testing.TB, network, iso string) []byte {
	t.Helper()
	ip := net.ParseIP(network).To4()
	if ip == nil {
		t.Fatalf("GeoIPDatabase: %q is not an IPv4 address", network)
	}
	const depth = 24 // a /24: one node per bit of the prefix
	nodeCount := uint32(depth)
	// The data: {"country": {"iso_code": iso}}.
	data := append([]byte{0xE1}, mmdbString("country")...)
	data = append(data, 0xE1)
	data = append(data, mmdbString("iso_code")...)
	data = append(data, mmdbString(iso)...)
	dataPointer := nodeCount + 16 // the record value pointing at data offset 0

	// The search tree: node i follows bit i of the prefix; the other branch is
	// empty (nodeCount), and the last node's branch points at the data.
	var tree []byte
	for i := 0; i < depth; i++ {
		bit := ip[i/8] >> (7 - uint(i%8)) & 1
		next := uint32(i + 1)
		if i == depth-1 {
			next = dataPointer
		}
		recs := [2]uint32{nodeCount, nodeCount}
		recs[bit] = next
		for _, r := range recs { // 24-bit records
			tree = append(tree, byte(r>>16), byte(r>>8), byte(r))
		}
	}

	meta := []byte{0xE9} // a map of 9 entries
	for _, kv := range []struct {
		k string
		v []byte
	}{
		{"binary_format_major_version", mmdbUint16(2)},
		{"binary_format_minor_version", mmdbUint16(0)},
		{"build_epoch", []byte{0x00, 0x02}}, // uint64 0
		{"database_type", mmdbString("GeoLite2-Country")},
		{"description", []byte{0xE0}}, // empty map
		{"ip_version", mmdbUint16(4)},
		{"languages", []byte{0x00, 0x04}}, // empty array
		{"node_count", mmdbUint32(nodeCount)},
		{"record_size", mmdbUint16(24)},
	} {
		meta = append(meta, mmdbString(kv.k)...)
		meta = append(meta, kv.v...)
	}

	out := append(tree, make([]byte, 16)...) // the data section separator
	out = append(out, data...)
	out = append(out, "\xAB\xCD\xEFMaxMind.com"...)
	return append(out, meta...)
}

func mmdbString(s string) []byte { return append([]byte{0x40 | byte(len(s))}, s...) }

func mmdbUint16(v uint16) []byte {
	if v == 0 {
		return []byte{0xA0}
	}
	if v < 256 {
		return []byte{0xA1, byte(v)}
	}
	return []byte{0xA2, byte(v >> 8), byte(v)}
}

func mmdbUint32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return append([]byte{0xC4}, b...)
}
