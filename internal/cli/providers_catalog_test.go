package cli

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/provider"
	"github.com/smweber/devvm/internal/provider/fake"
)

func withCatalog(t *testing.T, a *App, extra string) *fake.Provider {
	t.Helper()
	p := withFakeHetzner(t, a, extra, false)
	p.Cat = fake.SampleCatalog()
	return p
}

// `providers types`: cheapest first, deprecated left out, --location keeps
// what is available there priced there, and the notes say what the prices
// include. It needs a token, never devvm.toml.
func TestProvidersTypes(t *testing.T) {
	a := newTestApp(t)
	p := withCatalog(t, a, "")
	writeDevvmTOML(t, a, "[machines.x]\nbogus = 1\n") // a broken file is not this command's business

	if err := runTree(t, a, "providers", "types", "hetzner", "--location", "nbg1"); err != nil {
		t.Fatal(err)
	}
	out := a.Stdout.(*bytes.Buffer).String()
	if rows := tableNames(out); !slices.Equal(rows, []string{"cx23", "cax11", "ccx13"}) {
		t.Errorf("nbg1 rows = %v\n%s", rows, out)
	}
	for _, want := range []string{
		"Hetzner server types in nbg1 (Nuremberg, DE), cheapest first:",
		"EUR/MONTH", "shared arm", "12.49", "0.0056",
		"This account is charged no VAT (rate 0%)",
		"A public IPv4 address is billed on top: EUR 0.50/month (0.0008/hour) in nbg1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "cx22") || strings.Contains(out, "*") {
		t.Errorf("a deprecated type, or a 'from' mark with a location:\n%s", out)
	}

	// Every location: a sold-out type is still listed, with no locations,
	// and a price that differs by location is marked.
	if err := runTree(t, a, "providers", "types", "hetzner"); err != nil {
		t.Fatal(err)
	}
	out = a.Stdout.(*bytes.Buffer).String()
	if rows := tableNames(out); !slices.Equal(rows, []string{"cx23", "cax11", "cx33", "cx43", "ccx13"}) {
		t.Errorf("rows = %v\n%s", rows, out)
	}
	for _, want := range []string{"12.49*", "fsn1 nbg1 hel1 ash", "* the lowest of the type's locations", "- not available in any location"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	// VAT charged: the rate is said.
	p.Cat.VATRate = 19
	runTree(t, a, "providers", "types", "hetzner")
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "Prices include VAT at this account's rate (19%).") {
		t.Errorf("vat note:\n%s", out)
	}

	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"providers", "types", "hetzner", "--location", "mars"}, "Hetzner has no location 'mars' (it has: fsn1, nbg1, hel1, ash)"},
		{[]string{"providers", "types", "aws"}, `unknown provider "aws"`},
		{[]string{"providers", "types"}, "accepts 1 arg"},
	} {
		if err := runTree(t, a, tc.argv...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.argv, err, tc.want)
		}
	}
	p.CatalogErr = errors.New("503 service unavailable")
	if err := runTree(t, a, "providers", "types", "hetzner"); err == nil || !strings.Contains(err.Error(), "hetzner: listing server types: 503") {
		t.Errorf("api error: %v", err)
	}
	// No token: the existing wording, nothing dialled.
	p.CatalogErr = nil
	os.Remove(provider.TokenPath(a.Paths, "hetzner"))
	n := p.CallCount("Catalog")
	if err := runTree(t, a, "providers", "types", "hetzner"); err == nil || !strings.Contains(err.Error(), "devvm providers token hetzner") {
		t.Errorf("no token: %v", err)
	}
	if p.CallCount("Catalog") != n {
		t.Error("no token dialled the API")
	}
}

// tableNames is the first word of each table row (between the header and
// the blank line).
func tableNames(out string) []string {
	var names []string
	in := false
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "NAME"):
			in = true
		case l == "":
			in = false
		case in:
			names = append(names, strings.Fields(l)[0])
		}
	}
	return names
}

// --plain: the documented ten columns, nothing else on stdout.
func TestProvidersTypesPlain(t *testing.T) {
	a := newTestApp(t)
	withCatalog(t, a, "")
	if err := runTree(t, a, "providers", "types", "hetzner", "--plain", "--location", "fsn1"); err != nil {
		t.Fatal(err)
	}
	want := "cax11\tshared\tarm\t2\t4\t40\t3.79\t0.0061\tEUR\tfsn1,nbg1\n" +
		"cx43\tshared\tx86\t8\t16\t160\t9.49\t0.0152\tEUR\tfsn1\n" +
		"ccx13\tdedicated\tx86\t2\t8\t80\t12.49\t0.02\tEUR\tfsn1,nbg1,hel1,ash\n"
	if got := a.Stdout.(*bytes.Buffer).String(); got != want {
		t.Errorf("plain =\n%q\nwant\n%q", got, want)
	}
	runTree(t, a, "providers", "types", "hetzner", "--plain")
	for _, l := range strings.Split(strings.TrimSpace(a.Stdout.(*bytes.Buffer).String()), "\n") {
		f := strings.Split(l, "\t")
		if len(f) != 10 {
			t.Errorf("%d columns: %q", len(f), l)
		}
		if f[0] == "cx33" && f[9] != "-" {
			t.Errorf("sold out everywhere: %q", l)
		}
	}
}

// create --yes refuses, before the key upload and Create, a placement the
// catalogue says the API would refuse, naming the cheapest types there
// and the listing; a placement it accepts goes on.
func TestProviderCreateChecksPlacement(t *testing.T) {
	a := newTestApp(t)
	a.Stdin = strings.NewReader("")
	fakeLocalSmolvm(t, `[]`)
	fakeSSH(t, "exit 0")
	p := withCatalog(t, a, "ssh_key = \"scott\"\n")
	p.AddKey("scott", testPubKey)
	spy := useSpy(a, p)

	for _, tc := range []struct {
		argv []string
		want []string
	}{
		// providersTOML's fsn1 with the built-in cx23: sold out there.
		{[]string{"--yes"}, []string{"server type 'cx23' is not available in fsn1 (it is available in nbg1, hel1)",
			"the cheapest there: cax11 EUR 3.79/mo, cx43 EUR 9.49/mo, ccx13 EUR 12.49/mo",
			"see 'devvm providers types hetzner --location fsn1'"}},
		{[]string{"--yes", "--server-type", "cx99", "--location", "nbg1"}, []string{"Hetzner has no server type 'cx99'; the cheapest there: cx23"}},
		{[]string{"--yes", "--server-type", "cx33", "--location", "nbg1"}, []string{"(it is not available anywhere right now)"}},
		{[]string{"--yes", "--location", "mars"}, []string{"Hetzner has no location 'mars' (it has: fsn1, nbg1, hel1, ash)"}},
	} {
		err := runTree(t, a, append([]string{"create", "web", "--backend", "hetzner"}, tc.argv...)...)
		for _, w := range tc.want {
			if err == nil || !strings.Contains(err.Error(), w) {
				t.Errorf("%v: %v, want %q", tc.argv, err, w)
			}
		}
		if err != nil && strings.Contains(err.Error(), "\n") {
			t.Errorf("%v: not one line: %v", tc.argv, err)
		}
	}
	if p.CallCount("Create") != 0 || p.CallCount("FindSSHKey") != 0 {
		t.Fatalf("a refused placement reached Create %d / the key lookup %d", p.CallCount("Create"), p.CallCount("FindSSHKey"))
	}

	// Accepted (a deprecated type still available counts), and with the
	// catalogue down the API decides, silently.
	spy.createErr = errors.New("stop here")
	for _, argv := range [][]string{
		{"--yes", "--location", "nbg1"},
		{"--yes", "--server-type", "cx22", "--location", "nbg1"},
	} {
		if err := runTree(t, a, append([]string{"create", "web", "--backend", "hetzner"}, argv...)...); err == nil || !strings.Contains(err.Error(), "stop here") {
			t.Errorf("%v: %v, want it to reach Create", argv, err)
		}
	}
	p.CatalogErr = errors.New("dial tcp: no route to host")
	if err := runTree(t, a, "create", "web", "--backend", "hetzner", "--yes"); err == nil || !strings.Contains(err.Error(), "stop here") {
		t.Errorf("catalogue down: %v, want it to reach Create", err)
	}
	if s := a.Stderr.(*bytes.Buffer).String(); strings.Contains(s, "route") {
		t.Errorf("a non-interactive create spoke of the catalogue: %q", s)
	}
}

// On a terminal: the catalogue's selects when it can be fetched, then the
// check; the text inputs with one stderr line when it cannot; nothing said
// when every field came by flag.
func TestProviderCreatePickerOrFallback(t *testing.T) {
	a := newTestApp(t)
	p := withCatalog(t, a, "")
	tty, err := os.CreateTemp(t.TempDir(), "tty")
	if err != nil {
		t.Fatal(err)
	}
	oldTTY, oldPicks, oldText := openTTY, askPlacementPicks, askPlacementText
	t.Cleanup(func() { openTTY, askPlacementPicks, askPlacementText = oldTTY, oldPicks, oldText })
	openTTY = func() *os.File { f, _ := os.Open(tty.Name()); return f }
	stop := errors.New("form answered")
	var picked, typed int
	var pickTo config.Placement
	askPlacementPicks = func(_ *os.File, pn string, _ *createSpec, pl *config.Placement, cat *provider.Catalog) error {
		picked++
		if pn != config.BackendHetzner || cat == nil {
			t.Errorf("picks(%q, %v)", pn, cat)
		}
		if pickTo.Location != "" {
			pl.Location, pl.ServerType = pickTo.Location, pickTo.ServerType
			return nil // on to the check
		}
		return stop
	}
	askPlacementText = func(*os.File, *createSpec, *config.Placement) error { typed++; return stop }
	gather := func(s createSpec) (string, error) {
		var w bytes.Buffer
		a.Stderr = &w
		s.Name, s.Backend = "web", config.BackendHetzner
		_, _, err := a.gatherProviderSpec(&s, config.ProviderSpec{Location: "fsn1"}, p)
		return w.String(), err
	}

	if line, err := gather(createSpec{}); err != stop || picked != 1 || typed != 0 || line != "" {
		t.Errorf("catalogue: err %v picked %d typed %d stderr %q", err, picked, typed, line)
	}
	// A pick is checked like a flag (the picker only offers valid ones; a
	// flag-given type it filtered by may not be).
	pickTo = config.Placement{Location: "hel1", ServerType: "cx43"}
	if _, err := gather(createSpec{}); err == nil || !strings.Contains(err.Error(), "'cx43' is not available in hel1") {
		t.Errorf("an invalid pick: %v", err)
	}
	pickTo = config.Placement{}

	p.CatalogErr = errors.New("context deadline exceeded")
	line, err := gather(createSpec{})
	if err != stop || typed != 1 {
		t.Errorf("catalogue down: err %v typed %d", err, typed)
	}
	if want := "devvm: cannot offer Hetzner's server types and locations to pick from (hetzner: listing server types: context deadline exceeded); type them instead\n"; line != want {
		t.Errorf("stderr = %q, want %q", line, want)
	}
	// Type and location by flag: only the image is asked, and no word of
	// the catalogue.
	if line, err := gather(createSpec{ServerType: "cx23", Location: "nbg1"}); err != stop || line != "" {
		t.Errorf("flags: err %v stderr %q", err, line)
	}
	// --yes: no form at all.
	picked, typed = 0, 0
	p.CatalogErr = nil
	if _, err := gather(createSpec{Yes: true, Location: "nbg1"}); err != nil || picked+typed != 0 {
		t.Errorf("--yes: err %v, forms %d", err, picked+typed)
	}
}

// The picker's content, built as data: locations with something on offer
// (or where a flag-given type is), types there cheapest first with their
// specs and price, and the seed kept only when it is offered.
func TestPickerOptions(t *testing.T) {
	cat := fake.SampleCatalog()
	locs := locationOptions(cat, "")
	if v := optionValues(locs); !slices.Equal(v, []string{"fsn1", "nbg1", "hel1", "ash"}) {
		t.Errorf("locations = %v", v)
	}
	// Aligned columns: "ash" is padded to "fsn1", every city to the longest.
	if locs[1].Label != "nbg1  Nuremberg, DE    (3 types, from EUR 3.49/mo)" {
		t.Errorf("nbg1 label = %q", locs[1].Label)
	}
	if !strings.HasPrefix(locs[3].Label, "ash   Ashburn, VA, US  (") {
		t.Errorf("ash label = %q", locs[3].Label)
	}
	if v := optionValues(locationOptions(cat, "cx43")); !slices.Equal(v, []string{"fsn1"}) {
		t.Errorf("locations for cx43 = %v", v)
	}
	if got := locationOptions(cat, "cx43")[0].Label; !strings.Contains(got, "(cx43 EUR 9.49/mo)") {
		t.Errorf("cx43 location label = %q", got)
	}
	if len(locationOptions(cat, "nope")) != 0 {
		t.Error("an unknown type has locations")
	}

	types := typeOptions(cat, "nbg1")
	if v := optionValues(types); !slices.Equal(v, []string{"cx23", "cax11", "ccx13"}) {
		t.Errorf("types = %v (cheapest first, no deprecated cx22)", v)
	}
	if want := "cx23   shared x86      2 cores     4 GB RAM    40 GB disk  EUR 3.49/mo"; types[0].Label != want {
		t.Errorf("label =\n%q\nwant\n%q", types[0].Label, want)
	}
	one := &provider.Catalog{Currency: "EUR", Types: []provider.ServerType{{Name: "cpx02", CPU: "shared", Arch: "x86", Cores: 1, MemoryGB: 1, DiskGB: 20,
		Available: []string{"hel1"}, Prices: map[string]provider.Price{"hel1": {Monthly: 4.99}}}}}
	if got := typeOptions(one, "hel1")[0].Label; got != "cpx02  shared x86   1 core      1 GB RAM    20 GB disk  EUR 4.99/mo" {
		t.Errorf("one-core label = %q", got)
	}
	if len(typeOptions(cat, "mars")) != 0 {
		t.Error("an unknown location has types")
	}
	for _, tc := range []struct{ want, seed string }{{"ccx13", "ccx13"}, {"cx43", "cx23"}, {"", "cx23"}} {
		if got := seedPick(types, tc.want); got != tc.seed {
			t.Errorf("seedPick(%q) = %q, want %q", tc.want, got, tc.seed)
		}
	}
	if seedPick(nil, "x") != "" {
		t.Error("seedPick of nothing")
	}
}

func optionValues(opts []pickOption) []string {
	out := make([]string, len(opts))
	for i, o := range opts {
		out[i] = o.Value
	}
	return out
}

// Offered and PriceIn, the order everything above relies on.
func TestCatalogOrdering(t *testing.T) {
	cat := fake.SampleCatalog()
	var names []string
	for _, st := range cat.Offered("") {
		names = append(names, st.Name)
	}
	if !slices.Equal(names, []string{"cx23", "cax11", "cx33", "cx43", "ccx13"}) {
		t.Errorf("offered = %v", names)
	}
	ccx, _ := cat.Type("ccx13")
	if p, _ := ccx.PriceIn(""); p.Monthly != 12.49 || !ccx.PriceVaries() {
		t.Errorf("ccx13 from %v varies %v", p, ccx.PriceVaries())
	}
	if p, _ := ccx.PriceIn("ash"); p.Monthly != 13.99 {
		t.Errorf("ccx13 in ash = %v", p)
	}
	if p, ok := (provider.ServerType{}).PriceIn(""); ok || p.Monthly != 0 {
		t.Error("an unpriced type has a price")
	}
}
