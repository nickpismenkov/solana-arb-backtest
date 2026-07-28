package signal

import "testing"

func TestNoSpreadIsNegativeAfterFees(t *testing.T) {
	_, edge := LocalEdge(100.0, 100.0, 4.0, 4.0)
	if !(edge < 0.0 && edge > -10.0) {
		t.Fatalf("edge %v out of expected range", edge)
	}
}

func TestRayHigherFavorsOrcaFirst(t *testing.T) {
	orcaFirst, edge := LocalEdge(100.0, 100.5, 1.0, 1.0)
	if !orcaFirst {
		t.Fatal("expected orcaFirst=true")
	}
	if edge <= 0.0 {
		t.Fatalf("edge %v expected > 0", edge)
	}
}

func TestOrcaHigherFavorsRayFirst(t *testing.T) {
	orcaFirst, edge := LocalEdge(100.5, 100.0, 1.0, 1.0)
	if orcaFirst {
		t.Fatal("expected orcaFirst=false")
	}
	if edge <= 0.0 {
		t.Fatalf("edge %v expected > 0", edge)
	}
}

func TestCacheRoundtrips(t *testing.T) {
	var c PriceCache
	c.SetOrca(123.45, 10)
	c.SetRay(543.21, 11)
	o, r, os, rs := c.Get()
	if o != 123.45 || r != 543.21 || os != 10 || rs != 11 {
		t.Fatalf("roundtrip mismatch: %v %v %v %v", o, r, os, rs)
	}
}
