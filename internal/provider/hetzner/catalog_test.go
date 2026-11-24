package hetzner

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

// The catalogue against the recorded answers: both list endpoints followed
// across pages, availability from each type's locations[].available (a
// type phased out of a location, or sold out there, is priced but not
// available), prices gross, and IPv4 priced apart from the type.
func TestCatalogRecorded(t *testing.T) {
	cat, err := replay(t, "catalog", WithPerPage(3)).Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cat.Currency != "EUR" || cat.VATRate != 0 {
		t.Errorf("currency %q vat %v", cat.Currency, cat.VATRate)
	}
	var locs []string
	for _, l := range cat.Locations {
		locs = append(locs, l.Name)
	}
	if !slices.Equal(locs, []string{"fsn1", "nbg1", "hel1", "ash", "hil", "sin"}) {
		t.Errorf("locations = %v", locs)
	}
	if l, _ := cat.Location("nbg1"); l.City != "Nuremberg" || l.Country != "DE" {
		t.Errorf("nbg1 = %+v", l)
	}
	if len(cat.Types) != 6 {
		t.Fatalf("%d types, want 6 (two pages)", len(cat.Types))
	}
	cx23, ok := cat.Type("cx23")
	if !ok || cx23.CPU != "shared" || cx23.Arch != "x86" || cx23.Cores != 2 || cx23.MemoryGB != 4 || cx23.DiskGB != 40 || cx23.Deprecated {
		t.Fatalf("cx23 = %+v", cx23)
	}
	// Sold out in fsn1 at the recording, still priced there.
	if !slices.Equal(cx23.Available, []string{"nbg1", "hel1"}) || cx23.AvailableIn("fsn1") {
		t.Errorf("cx23 available = %v", cx23.Available)
	}
	if _, ok := cx23.Prices["fsn1"]; !ok {
		t.Error("cx23 lost its fsn1 price")
	}
	if p := cx23.Prices["nbg1"]; p.Monthly <= 0 || p.Hourly <= 0 || p.Hourly >= p.Monthly {
		t.Errorf("cx23 nbg1 price = %+v", p)
	}
	cpx11, _ := cat.Type("cpx11") // phased out of the EU locations (deprecation per location)
	if !slices.Equal(cpx11.Available, []string{"ash", "hil"}) {
		t.Errorf("cpx11 available = %v", cpx11.Available)
	}
	if cax, _ := cat.Type("cax11"); cax.Arch != "arm" || len(cax.Available) != 0 {
		t.Errorf("cax11 = %+v", cax)
	}
	if ccx, _ := cat.Type("ccx13"); ccx.CPU != "dedicated" || len(ccx.Available) != 6 {
		t.Errorf("ccx13 = %+v", ccx)
	}
	if p, ok := cat.IPv4["nbg1"]; !ok || p.Monthly != 0.5 || p.Hourly != 0.0008 {
		t.Errorf("ipv4 nbg1 = %+v %v", p, ok)
	}
	// The listing order: cheapest first in nbg1, only what is available there.
	var names []string
	for _, st := range cat.Offered("nbg1") {
		names = append(names, st.Name)
	}
	if !slices.Equal(names, []string{"cx23", "cpx12", "ccx13"}) {
		t.Errorf("offered in nbg1 = %v", names)
	}
}

// Deprecation, in both shapes the API has used, and VAT: gross prices are
// what the catalogue carries, so a VAT-charged account sees what it pays.
func TestBuildCatalogDeprecatedAndVAT(t *testing.T) {
	var types []apiServerType
	if err := json.Unmarshal([]byte(`[
	 {"name":"old","cores":1,"memory":1,"disk":20,"cpu_type":"shared","architecture":"x86","deprecated":true,"deprecation":null,
	  "locations":[{"name":"nbg1","available":true,"deprecation":null}],
	  "prices":[{"location":"nbg1","price_hourly":{"net":"0.01","gross":"0.0119"},"price_monthly":{"net":"5","gross":"5.95"}}]},
	 {"name":"going","cores":1,"memory":1,"disk":20,"cpu_type":"shared","architecture":"x86",
	  "deprecation":{"announced":"2026-01-01T00:00:00Z","unavailable_after":"2026-12-31T00:00:00Z"},
	  "locations":[{"name":"nbg1","available":true,"deprecation":null}],"prices":[]},
	 {"name":"cur","cores":2,"memory":0.5,"disk":20,"cpu_type":"dedicated","architecture":"arm","deprecation":null,
	  "locations":[{"name":"nbg1"},{"name":"hel1","deprecation":{"announced":"x"}}],"prices":[]}
	]`), &types); err != nil {
		t.Fatal(err)
	}
	var pr apiPricing
	pr.Pricing.Currency, pr.Pricing.VATRate = "EUR", "19.000000"
	cat, err := buildCatalog(types, nil, pr)
	if err != nil {
		t.Fatal(err)
	}
	if cat.VATRate != 19 || cat.IPv4 != nil {
		t.Errorf("vat %v ipv4 %v", cat.VATRate, cat.IPv4)
	}
	if o, _ := cat.Type("old"); !o.Deprecated || o.Prices["nbg1"].Monthly != 5.95 {
		t.Errorf("old = %+v (want deprecated, gross 5.95)", o)
	}
	if g, _ := cat.Type("going"); !g.Deprecated || !g.AvailableIn("nbg1") {
		t.Errorf("going = %+v (deprecated, still available)", g)
	}
	// No available flag: a listed location counts unless it is phased out.
	if c, _ := cat.Type("cur"); c.Deprecated || !slices.Equal(c.Available, []string{"nbg1"}) || c.MemoryGB != 0.5 {
		t.Errorf("cur = %+v", c)
	}
	if off := cat.Offered(""); len(off) != 1 || off[0].Name != "cur" {
		t.Errorf("offered = %+v, want only the current type", off)
	}
	var bad apiPricing
	bad.Pricing.VATRate = "lots"
	if _, err := buildCatalog(nil, nil, bad); err == nil {
		t.Error("a bad vat_rate parsed")
	}
}

// An API error on any of the three calls is the catalogue's error.
func TestCatalogError(t *testing.T) {
	if _, err := replay(t, "unauthorized_catalog").Catalog(context.Background()); !IsCode(err, "unauthorized") {
		t.Errorf("err = %v", err)
	}
}
