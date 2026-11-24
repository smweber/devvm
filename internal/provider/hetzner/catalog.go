package hetzner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/smweber/devvm/internal/provider"
)

// The catalogue: three read-only GETs. Availability lives on each server
// type (locations[].available); the /datacenters endpoint that once carried
// it was removed (410 deprecated_api_endpoint, 2026-06-02). /pricing is the
// only place that says the currency, this account's VAT rate and what a
// primary IPv4 costs, which the server type's own price leaves out.

var _ provider.Cataloger = (*Client)(nil)

type apiPrice struct {
	Location     string    `json:"location"`
	PriceHourly  apiAmount `json:"price_hourly"`
	PriceMonthly apiAmount `json:"price_monthly"`
}

// apiAmount is a price as decimal strings; gross has this account's VAT
// added (vat_rate in /pricing), net has none.
type apiAmount struct {
	Gross string `json:"gross"`
	Net   string `json:"net"`
}

type apiServerType struct {
	Name         string     `json:"name"`
	Architecture string     `json:"architecture"`
	Cores        int        `json:"cores"`
	CPUType      string     `json:"cpu_type"`
	Memory       float64    `json:"memory"`
	Disk         int        `json:"disk"`
	Deprecated   *bool      `json:"deprecated"` // the older field; deprecation replaced it
	Deprecation  *struct{}  `json:"deprecation"`
	Prices       []apiPrice `json:"prices"`
	Locations    []struct {
		Name        string    `json:"name"`
		Available   *bool     `json:"available"`
		Deprecation *struct{} `json:"deprecation"`
	} `json:"locations"`
}

type apiLocation struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	City    string `json:"city"`
	Country string `json:"country"`
}

type apiPricing struct {
	Pricing struct {
		Currency   string `json:"currency"`
		VATRate    string `json:"vat_rate"`
		PrimaryIPs []struct {
			Type   string     `json:"type"`
			Prices []apiPrice `json:"prices"`
		} `json:"primary_ips"`
	} `json:"pricing"`
}

// Catalog fetches server types, locations and pricing. Prices are gross:
// what this account is billed, its VAT included.
func (c *Client) Catalog(ctx context.Context) (*provider.Catalog, error) {
	types, err := getAll[apiServerType](ctx, c, "/server_types", "server_types")
	if err != nil {
		return nil, err
	}
	locs, err := getAll[apiLocation](ctx, c, "/locations", "locations")
	if err != nil {
		return nil, err
	}
	var pr apiPricing
	if err := c.do(ctx, http.MethodGet, "/pricing", nil, &pr); err != nil {
		return nil, err
	}
	return buildCatalog(types, locs, pr)
}

// buildCatalog maps the wire shapes; split from Catalog so the recorded
// fixtures are parsed without a server.
func buildCatalog(types []apiServerType, locs []apiLocation, pr apiPricing) (*provider.Catalog, error) {
	cat := &provider.Catalog{Currency: pr.Pricing.Currency}
	if pr.Pricing.VATRate != "" {
		v, err := strconv.ParseFloat(pr.Pricing.VATRate, 64)
		if err != nil {
			return nil, fmt.Errorf("hetzner: GET /pricing: vat_rate %q: %w", pr.Pricing.VATRate, err)
		}
		cat.VATRate = v
	}
	for _, l := range locs {
		cat.Locations = append(cat.Locations, provider.Location{Name: l.Name, City: l.City, Country: l.Country})
	}
	for _, ip := range pr.Pricing.PrimaryIPs {
		if ip.Type != "ipv4" {
			continue
		}
		m, err := priceMap(ip.Prices)
		if err != nil {
			return nil, fmt.Errorf("hetzner: GET /pricing: primary ipv4: %w", err)
		}
		cat.IPv4 = m
	}
	for _, t := range types {
		st := provider.ServerType{
			Name: t.Name, CPU: t.CPUType, Arch: t.Architecture,
			Cores: t.Cores, MemoryGB: t.Memory, DiskGB: t.Disk,
			Deprecated: t.Deprecation != nil || (t.Deprecated != nil && *t.Deprecated),
		}
		for _, l := range t.Locations {
			// An answer without the flag (an older shape) counts a listed,
			// not phased-out location as available; the API decides at Create.
			if (l.Available != nil && *l.Available) || (l.Available == nil && l.Deprecation == nil) {
				st.Available = append(st.Available, l.Name)
			}
		}
		var err error
		if st.Prices, err = priceMap(t.Prices); err != nil {
			return nil, fmt.Errorf("hetzner: GET /server_types: %s: %w", t.Name, err)
		}
		cat.Types = append(cat.Types, st)
	}
	return cat, nil
}

func priceMap(ps []apiPrice) (map[string]provider.Price, error) {
	out := make(map[string]provider.Price, len(ps))
	for _, p := range ps {
		m, err := strconv.ParseFloat(p.PriceMonthly.Gross, 64)
		if err != nil {
			return nil, fmt.Errorf("monthly price in %s: %w", p.Location, err)
		}
		h, err := strconv.ParseFloat(p.PriceHourly.Gross, 64)
		if err != nil {
			return nil, fmt.Errorf("hourly price in %s: %w", p.Location, err)
		}
		out[p.Location] = provider.Price{Monthly: m, Hourly: h}
	}
	return out, nil
}

// getAll GETs every page of a list endpoint and returns the items under
// key, following meta.pagination.next_page as ListSelector does.
func getAll[T any](ctx context.Context, c *Client, path, key string) ([]T, error) {
	var out []T
	for page := 1; ; {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("per_page", strconv.Itoa(c.perPage))
		var resp map[string]json.RawMessage
		if err := c.do(ctx, http.MethodGet, path+"?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		var items []T
		if raw, ok := resp[key]; ok {
			if err := json.Unmarshal(raw, &items); err != nil {
				return nil, fmt.Errorf("hetzner: GET %s: decoding response: %w", path, err)
			}
		}
		out = append(out, items...)
		var meta struct {
			Pagination struct {
				NextPage *int `json:"next_page"`
			} `json:"pagination"`
		}
		if raw, ok := resp["meta"]; ok {
			if err := json.Unmarshal(raw, &meta); err != nil {
				return nil, fmt.Errorf("hetzner: GET %s: decoding pagination: %w", path, err)
			}
		}
		next := meta.Pagination.NextPage
		if next == nil || *next <= page {
			return out, nil
		}
		page = *next
	}
}
