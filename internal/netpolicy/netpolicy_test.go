package netpolicy

import (
	"net/netip"
	"testing"
)

var ianaSpecialPurpose = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10",
	"127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16",
	"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"192.31.196.0/24", "192.52.193.0/24", "192.88.99.0/24", "192.175.48.0/24",
	"::/128", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "100:0:0:1::/64", "2001::/23",
	"2001:2::/48", "2001:3::/32", "2001:4:112::/48", "2001:10::/28", "2001:20::/28", "2001:30::/28",
	"2001:db8::/32", "2002::/16", "2620:4f:8000::/48", "3fff::/20", "5f00::/16", "fc00::/7",
	"fe80::/10", "ff00::/8",
}

func TestTableIsTheAdversariallyVerifiedSpecialPurposeList(t *testing.T) {
	if len(nonPublicPrefixes) != len(ianaSpecialPurpose) {
		t.Fatalf("table has %d prefixes, want %d", len(nonPublicPrefixes), len(ianaSpecialPurpose))
	}
	for i, want := range ianaSpecialPurpose {
		if nonPublicPrefixes[i] != netip.MustParsePrefix(want) {
			t.Errorf("prefix %d is %s, want %s", i, nonPublicPrefixes[i], want)
		}
	}
}

// How httpfetch and oci both spelled this, over the pinned table above.
func predicateBeforeExtraction(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() {
		return false
	}
	if addr.Is6() && !netip.MustParsePrefix("2000::/3").Contains(addr) {
		return false
	}
	for _, raw := range ianaSpecialPurpose {
		if netip.MustParsePrefix(raw).Contains(addr) {
			return false
		}
	}
	return true
}

func TestPublicAnswersAsTheAdapterPredicatesDid(t *testing.T) {
	corpus := addressProbe()
	if len(corpus) < 200 {
		t.Fatalf("probe built only %d addresses", len(corpus))
	}
	accepted := 0
	for _, addr := range corpus {
		got := Public(addr)
		if want := predicateBeforeExtraction(addr); got != want {
			t.Errorf("Public(%v) = %v, the extracted predicate said %v", addr, got, want)
		}
		if got {
			accepted++
		}
	}
	if accepted == 0 || accepted == len(corpus) {
		t.Fatalf("probe is degenerate: %d of %d accepted", accepted, len(corpus))
	}
}

func TestAZonedAddressIsNotPublicEvenInsideGlobalUnicast(t *testing.T) {
	for _, raw := range []string{"2606:4700:4700::1111%lo0", "2001:4860:4860::8888%eth0", "2001::1%lo0"} {
		if Public(netip.MustParseAddr(raw)) {
			t.Errorf("zoned %s accepted", raw)
		}
	}
	if !Public(netip.MustParseAddr("2606:4700:4700::1111")) {
		t.Fatal("the unzoned form is refused too, so the zoned cases prove nothing")
	}
}

func addressProbe() []netip.Addr {
	probe := []netip.Addr{
		{},
		netip.MustParseAddr("8.8.8.8"),
		netip.MustParseAddr("1.1.1.1"),
		netip.MustParseAddr("93.184.216.34"),
		netip.MustParseAddr("::ffff:8.8.8.8"),
		netip.MustParseAddr("::ffff:127.0.0.1"),
		netip.MustParseAddr("::ffff:169.254.169.254"),
		netip.MustParseAddr("2001:4860:4860::8888"),
		netip.MustParseAddr("2606:4700:4700::1111"),
		netip.MustParseAddr("fe80::1%eth0"),
		netip.MustParseAddr("ff02::1"),
		netip.MustParseAddr("224.0.0.1"),
		netip.MustParseAddr("255.255.255.255"),
		netip.MustParseAddr("4000::1"),
		netip.MustParseAddr("2000::"),
		netip.MustParseAddr("3fff:ffff::1"),
	}
	for _, raw := range ianaSpecialPurpose {
		prefix := netip.MustParsePrefix(raw)
		first, last := prefix.Addr(), lastAddress(prefix)
		probe = append(probe, first, first.Next(), first.Prev(), last, last.Next())
		if first.Is4() {
			probe = append(probe, netip.AddrFrom16(first.As16()), netip.AddrFrom16(last.As16()))
		}
	}
	return probe
}

func lastAddress(prefix netip.Prefix) netip.Addr {
	if prefix.Addr().Is4() {
		octets := prefix.Addr().As4()
		for bit := prefix.Bits(); bit < 32; bit++ {
			octets[bit/8] |= 1 << (7 - bit%8)
		}
		return netip.AddrFrom4(octets)
	}
	octets := prefix.Addr().As16()
	for bit := prefix.Bits(); bit < 128; bit++ {
		octets[bit/8] |= 1 << (7 - bit%8)
	}
	return netip.AddrFrom16(octets)
}
