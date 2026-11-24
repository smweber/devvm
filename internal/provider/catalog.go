package provider

import (
	"context"
	"slices"
	"strings"
)

// Cataloger is the optional capability beside Provider: what the provider
// sells (server types, their specs and prices) and where (locations), so
// `providers types` can list it and `create` can offer it as a picker and
// refuse a placement the API would refuse, before anything bills. The cli
// asserts it on the client (as backend's Refresher); a client without it
// leaves create to the text inputs and the API's own answer.
type Cataloger interface {
	// Catalog is one live fetch; nothing caches it (it changes as types
	// sell out, and only a human's command asks for it).
	Catalog(ctx context.Context) (*Catalog, error)
}

// Catalog is the provider's offer as one command sees it.
type Catalog struct {
	// Currency of every price (Hetzner: "EUR").
	Currency string
	// VATRate is the VAT percent the provider charges this account; every
	// Price includes it (0: the prices are net and gross alike).
	VATRate float64
	// Locations in the provider's order (Hetzner's by id: the EU ones first).
	Locations []Location
	// Types in the provider's order, deprecated ones included: a deprecated
	// type may still be created, so the placement check knows it, but the
	// listing and the picker leave it out (Offered).
	Types []ServerType
	// IPv4 is what a public IPv4 address costs by location, billed apart
	// from the server type; nil when the provider prices none separately.
	IPv4 map[string]Price
}

// Location is one place a server can be made.
type Location struct {
	Name    string // "nbg1": what --location and the templates take
	City    string
	Country string // ISO code, "DE"
}

// ServerType is one size the provider sells.
type ServerType struct {
	Name       string // "cx23"
	CPU        string // "shared" or "dedicated"
	Arch       string // "x86" or "arm"
	Cores      int
	MemoryGB   float64
	DiskGB     int
	Deprecated bool
	// Available is where a server of this type can be created now, in the
	// provider's location order. A type sold out or phased out somewhere is
	// not available there, though it may still be priced there.
	Available []string
	Prices    map[string]Price // by location
}

// Price is one resource's cost in one location, VAT as Catalog.VATRate.
// Hetzner bills by the hour, capped at the monthly price.
type Price struct {
	Monthly float64
	Hourly  float64
}

// Location looks a location up by name.
func (c *Catalog) Location(name string) (Location, bool) {
	for _, l := range c.Locations {
		if l.Name == name {
			return l, true
		}
	}
	return Location{}, false
}

// Type looks a server type up by name, deprecated ones included.
func (c *Catalog) Type(name string) (ServerType, bool) {
	for _, t := range c.Types {
		if t.Name == name {
			return t, true
		}
	}
	return ServerType{}, false
}

// AvailableIn reports whether a server of this type can be created in loc.
func (t ServerType) AvailableIn(loc string) bool { return slices.Contains(t.Available, loc) }

// PriceIn is the type's price in loc, or, for loc "", the lowest among the
// locations it is available in (any priced one when it is available
// nowhere): the "from" price a listing without a location shows.
func (t ServerType) PriceIn(loc string) (Price, bool) {
	if loc != "" {
		p, ok := t.Prices[loc]
		return p, ok
	}
	var best Price
	found := false
	pick := func(locs []string) {
		for _, l := range locs {
			if p, ok := t.Prices[l]; ok && (!found || p.Monthly < best.Monthly) {
				best, found = p, true
			}
		}
	}
	pick(t.Available)
	if !found {
		locs := make([]string, 0, len(t.Prices))
		for l := range t.Prices {
			locs = append(locs, l)
		}
		slices.Sort(locs) // map order must not pick among equal prices
		pick(locs)
	}
	return best, found
}

// PriceVaries reports whether the type costs differently across the
// locations it is available in, so a "from" price needs saying.
func (t ServerType) PriceVaries() bool {
	first, seen := 0.0, false
	for _, l := range t.Available {
		p, ok := t.Prices[l]
		if !ok {
			continue
		}
		if seen && p.Monthly != first {
			return true
		}
		first, seen = p.Monthly, true
	}
	return false
}

// Offered is what a listing or a picker shows: the types not deprecated,
// cheapest first (by the price in loc, or the "from" price for loc ""),
// then fewer cores, less memory, by name. With loc, only the types
// available there; without, every one, available or not, so a type sold
// out everywhere is still seen with no locations.
func (c *Catalog) Offered(loc string) []ServerType {
	var out []ServerType
	for _, t := range c.Types {
		if t.Deprecated || (loc != "" && !t.AvailableIn(loc)) {
			continue
		}
		out = append(out, t)
	}
	slices.SortStableFunc(out, func(a, b ServerType) int {
		pa, _ := a.PriceIn(loc)
		pb, _ := b.PriceIn(loc)
		switch {
		case pa.Monthly != pb.Monthly:
			return cmpFloat(pa.Monthly, pb.Monthly)
		case a.Cores != b.Cores:
			return a.Cores - b.Cores
		case a.MemoryGB != b.MemoryGB:
			return cmpFloat(a.MemoryGB, b.MemoryGB)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// OfferedLocations is the locations where at least one offered type is
// available, in the provider's order; with typeName, only those where that
// type is available (deprecated or not: it was asked for by name).
func (c *Catalog) OfferedLocations(typeName string) []Location {
	var out []Location
	for _, l := range c.Locations {
		if typeName != "" {
			if t, ok := c.Type(typeName); ok && t.AvailableIn(l.Name) {
				out = append(out, l)
			}
			continue
		}
		if len(c.Offered(l.Name)) > 0 {
			out = append(out, l)
		}
	}
	return out
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
