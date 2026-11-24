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
	// Available is where a server of this type can be created now, and
	// SoldOut where it is offered but cannot be created now (it may come
	// back within the hour, or not), both in the provider's location order.
	// A location in neither does not offer the type (never did, or phased
	// it out), though it may still be priced there.
	Available []string
	SoldOut   []string
	Prices    map[string]Price // by location
}

// Status is a type's standing in one location.
type Status int

const (
	NotOffered Status = iota
	SoldOut
	Available
)

// StatusIn is the type's standing in loc.
func (t ServerType) StatusIn(loc string) Status {
	switch {
	case slices.Contains(t.Available, loc):
		return Available
	case slices.Contains(t.SoldOut, loc):
		return SoldOut
	}
	return NotOffered
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
func (t ServerType) AvailableIn(loc string) bool { return t.StatusIn(loc) == Available }

// PriceIn is the type's price in loc, or, for loc "", the lowest among the
// locations it is available in (else among those it is sold out in, else
// any priced one): the price a type is sorted by across locations.
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
		pick(t.SoldOut)
	}
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

// Offered is what a listing or a picker shows: the types not deprecated
// that are offered in loc (anywhere, for loc ""), available or sold out.
// The available ones come first (in loc; anywhere, for ""), each group
// cheapest first by PriceIn(loc), then fewer cores, less memory, by name:
// what can be had now leads, and a sold-out type is still seen.
func (c *Catalog) Offered(loc string) []ServerType {
	var out []ServerType
	for _, t := range c.Types {
		if t.Deprecated {
			continue
		}
		if (loc != "" && t.StatusIn(loc) == NotOffered) || (loc == "" && len(t.Available)+len(t.SoldOut) == 0) {
			continue
		}
		out = append(out, t)
	}
	avail := func(t ServerType) bool {
		if loc == "" {
			return len(t.Available) > 0
		}
		return t.AvailableIn(loc)
	}
	slices.SortStableFunc(out, func(a, b ServerType) int {
		pa, _ := a.PriceIn(loc)
		pb, _ := b.PriceIn(loc)
		switch {
		case avail(a) != avail(b):
			if avail(a) {
				return -1
			}
			return 1
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

// AvailableTypes is Offered(loc) less the sold-out ones: what a server can
// be made as in loc now, cheapest first.
func (c *Catalog) AvailableTypes(loc string) []ServerType {
	var out []ServerType
	for _, t := range c.Offered(loc) {
		if t.AvailableIn(loc) {
			out = append(out, t)
		}
	}
	return out
}

// OfferingLocations is the locations that offer at least one listed type,
// available or sold out: the columns of a listing across locations.
func (c *Catalog) OfferingLocations() []Location {
	var out []Location
	for _, l := range c.Locations {
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
