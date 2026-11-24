package cli

import (
	"bytes"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/provider"
	"github.com/smweber/devvm/internal/provider/fake"
	"github.com/smweber/devvm/internal/provider/hetzner"
)

func withCatalog(t *testing.T, a *App, extra string) *fake.Provider {
	t.Helper()
	p := withFakeHetzner(t, a, extra, false)
	p.Cat = fake.SampleCatalog()
	return p
}

var escapeCode = regexp.MustCompile("\x1b\\[[0-9;]*m")

// `providers types`: one table, a column per location; types available
// somewhere first, cheapest first, then those sold out everywhere; a
// sold-out cell in parentheses, a location not offering the type '-'; the
// legend and the notes under it. It needs a token, never devvm.toml.
func TestProvidersTypes(t *testing.T) {
	a := newTestApp(t)
	p := withCatalog(t, a, "")
	writeDevvmTOML(t, a, "[machines.x]\nbogus = 1\n") // a broken file is not this command's business

	if err := runTree(t, a, "providers", "types", "hetzner"); err != nil {
		t.Fatal(err)
	}
	out := a.Stdout.(*bytes.Buffer).String()
	if escapeCode.MatchString(out) {
		t.Errorf("escape codes in piped output:\n%q", out)
	}
	if rows := tableNames(out); !slices.Equal(rows, []string{"cx23", "cax11", "cx43", "ccx13", "cx33"}) {
		t.Errorf("rows = %v (cx33 sold out everywhere goes last; deprecated cx22 not at all)\n%s", rows, out)
	}
	for _, want := range []string{
		"Hetzner server types right now, available first, cheapest first:\n",
		"NAME   CPU            CORES    RAM    DISK   fsn1    nbg1    hel1     ash\n",
		// cx23: sold out in fsn1, not offered in ash.
		"cx23   shared x86         2   4 GB   40 GB  (3.49)   3.49    3.49       -\n",
		// cx43: priced in hel1 but not offered there.
		"cx43   shared x86         8  16 GB  160 GB   9.49   (9.49)      -       -\n",
		"cx33   shared x86         4   8 GB   80 GB  (5.49)  (5.49)  (5.49)      -\n",
		"\n\nEUR/month. (price) = maybe sold out, - = not offered.\n" +
			"Hourly prices: 'devvm providers types hetzner --location LOC'.\n" +
			"No VAT on this account (0%). Billed hourly, up to the monthly price.\n" +
			"A stopped server still bills. Public IPv4: EUR 0.50/month extra.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	// --location: every type offered there, the available ones first, a
	// STATUS, and where each is available now; a type not offered there
	// (cx43 in hel1) is left out.
	if err := runTree(t, a, "providers", "types", "hetzner", "--location", "nbg1"); err != nil {
		t.Fatal(err)
	}
	out = a.Stdout.(*bytes.Buffer).String()
	if rows := tableNames(out); !slices.Equal(rows, []string{"cx23", "cax11", "ccx13", "cx33", "cx43"}) {
		t.Errorf("nbg1 rows = %v\n%s", rows, out)
	}
	for _, want := range []string{
		"Hetzner server types offered in nbg1 (Nuremberg, DE) right now, available first, cheapest first:\n",
		"NAME   CPU            CORES    RAM    DISK  EUR/MONTH  EUR/HOUR  STATUS     AVAILABLE IN\n",
		"cx23   shared x86         2   4 GB   40 GB       3.49    0.0056  available  nbg1 hel1\n",
		"cx33   shared x86         4   8 GB   80 GB       5.49    0.0088  sold out   -\n",
		"cx43   shared x86         8  16 GB  160 GB       9.49    0.0152  sold out   fsn1",
		"sold out   fsn1\n\n" + // no note on sold out
			"No VAT on this account (0%). Billed hourly, up to the monthly price.\n" +
			"A stopped server still bills. Public IPv4 in nbg1: EUR 0.50/month extra.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	runTree(t, a, "providers", "types", "hetzner", "--location", "hel1")
	if rows := tableNames(a.Stdout.(*bytes.Buffer).String()); slices.Contains(rows, "cx43") || !slices.Contains(rows, "cx33") {
		t.Errorf("hel1 rows = %v: cx43 is not offered there, cx33 is (sold out)", rows)
	}

	// VAT charged: the rate is said.
	p.Cat.VATRate = 19
	runTree(t, a, "providers", "types", "hetzner")
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "VAT included (19%). Billed hourly") {
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

// On a terminal: the header, type names and title bold, a row's cheapest
// available price in the accent, sold-out cells, '-' and the notes faint,
// a row sold out everywhere faint throughout (sold-out rows of --location
// too, with 'available' in the accent). With the codes stripped it is the
// plain output byte for byte, so the padding sits outside them.
func TestProvidersTypesStyled(t *testing.T) {
	cat := fake.SampleCatalog()
	on := styler{on: true}
	for name, pair := range map[string][2][]string{
		"grid":     {typeGrid(styler{}, "hetzner", cat), typeGrid(on, "hetzner", cat)},
		"location": {typeTable(styler{}, "hetzner", cat, "nbg1"), typeTable(on, "hetzner", cat, "nbg1")},
	} {
		plainOut, styled := strings.Join(pair[0], "\n"), strings.Join(pair[1], "\n")
		if escapeCode.MatchString(plainOut) || !escapeCode.MatchString(styled) {
			t.Fatalf("%s: plain has codes, or styled has none", name)
		}
		if got := escapeCode.ReplaceAllString(styled, ""); got != plainOut {
			t.Errorf("%s: styled, stripped, differs from plain:\n%s\n---\n%s", name, got, plainOut)
		}
	}
	grid := strings.Join(typeGrid(on, "hetzner", cat), "\n")
	for _, want := range []string{
		"\x1b[1mHetzner server types right now",
		"\x1b[1mNAME\x1b[0m", "\x1b[1mfsn1\x1b[0m",
		"\x1b[1mcx23\x1b[0m   shared x86         2   4 GB   40 GB  \x1b[2m(3.49)\x1b[0m   \x1b[32m3.49\x1b[0m    \x1b[32m3.49\x1b[0m       \x1b[2m-\x1b[0m\n",
		"  \x1b[32m12.49\x1b[0m   \x1b[32m12.49\x1b[0m   \x1b[32m12.49\x1b[0m   13.99\n", // ccx13: ash costs more
		"\x1b[2mcx33\x1b[0m   \x1b[2mshared x86\x1b[0m",
		"\x1b[2mEUR/month. (price) = maybe sold out",
	} {
		if !strings.Contains(grid, want) {
			t.Errorf("grid lacks %q:\n%q", want, grid)
		}
	}
	loc := strings.Join(typeTable(on, "hetzner", cat, "nbg1"), "\n")
	for _, want := range []string{"\x1b[32mavailable\x1b[0m", "\x1b[2msold out\x1b[0m", "\x1b[2mcx43\x1b[0m"} {
		if !strings.Contains(loc, want) {
			t.Errorf("--location lacks %q:\n%q", want, loc)
		}
	}
	// A file is not a terminal, whatever NO_COLOR says.
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if styleFor(f).on || styleFor(&bytes.Buffer{}).on {
		t.Error("styled output to a file or a buffer")
	}
}

// --plain: a line per type and location offering it, eleven columns,
// status available|soldout, a location not offering the type absent.
func TestProvidersTypesPlain(t *testing.T) {
	a := newTestApp(t)
	withCatalog(t, a, "")
	if err := runTree(t, a, "providers", "types", "hetzner", "--plain", "--location", "fsn1"); err != nil {
		t.Fatal(err)
	}
	want := "cax11\tshared\tarm\t2\t4\t40\tfsn1\tavailable\t3.79\t0.0061\tEUR\n" +
		"cx43\tshared\tx86\t8\t16\t160\tfsn1\tavailable\t9.49\t0.0152\tEUR\n" +
		"ccx13\tdedicated\tx86\t2\t8\t80\tfsn1\tavailable\t12.49\t0.02\tEUR\n" +
		"cx23\tshared\tx86\t2\t4\t40\tfsn1\tsoldout\t3.49\t0.0056\tEUR\n" +
		"cx33\tshared\tx86\t4\t8\t80\tfsn1\tsoldout\t5.49\t0.0088\tEUR\n"
	if got := a.Stdout.(*bytes.Buffer).String(); got != want {
		t.Errorf("plain =\n%q\nwant\n%q", got, want)
	}
	runTree(t, a, "providers", "types", "hetzner", "--plain")
	var cx43 []string
	for _, l := range strings.Split(strings.TrimSpace(a.Stdout.(*bytes.Buffer).String()), "\n") {
		f := strings.Split(l, "\t")
		if len(f) != 11 {
			t.Errorf("%d columns: %q", len(f), l)
		}
		if f[0] == "cx43" {
			cx43 = append(cx43, f[6]+"="+f[7])
		}
		if escapeCode.MatchString(l) {
			t.Errorf("escape code: %q", l)
		}
	}
	if !slices.Equal(cx43, []string{"fsn1=available", "nbg1=soldout"}) {
		t.Errorf("cx43 lines = %v (hel1 prices it but does not offer it)", cx43)
	}
}

// create --yes refuses, before the key upload and Create, what the API
// would refuse for good: an unknown location or type, a type not offered
// in the location. A type listed as sold out there is one stderr line and
// goes on to Create; a refusal there that is about availability gets
// where the type is available and the cheapest in the location appended.
func TestProviderCreateChecksPlacement(t *testing.T) {
	a := newTestApp(t)
	a.Stdin = strings.NewReader("")
	fakeLocalSmolvm(t, `[]`)
	fakeSSH(t, "exit 0")
	p := withCatalog(t, a, "ssh_key = \"scott\"\n")
	p.AddKey("scott", testPubKey)
	spy := useSpy(a, p)
	create := func(argv ...string) error {
		return runTree(t, a, append([]string{"create", "web", "--backend", "hetzner", "--yes"}, argv...)...)
	}

	for _, tc := range []struct {
		argv []string
		want []string
	}{
		{[]string{"--server-type", "cx43", "--location", "hel1"}, []string{"server type 'cx43' is not offered in hel1 (it is available in fsn1 right now)",
			"the cheapest available there: cx23 EUR 3.49/mo, ccx13 EUR 12.49/mo", "see 'devvm providers types hetzner --location hel1'"}},
		{[]string{"--location", "ash"}, []string{"server type 'cx23' is not offered in ash (it is available in nbg1, hel1 right now); the cheapest available there: ccx13 EUR 13.99/mo"}},
		{[]string{"--server-type", "cx99", "--location", "nbg1"}, []string{"Hetzner has no server type 'cx99'; the cheapest available there: cx23"}},
		{[]string{"--location", "mars"}, []string{"Hetzner has no location 'mars' (it has: fsn1, nbg1, hel1, ash)"}},
	} {
		err := create(tc.argv...)
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

	// providersTOML's fsn1 with the built-in cx23: sold out there. A
	// warning, Create, and the fake refusing it as the API does: the hint.
	err := create()
	if s := a.Stderr.(*bytes.Buffer).String(); s != "devvm: cx23 is listed as sold out in fsn1; trying anyway\n" {
		t.Errorf("stderr = %q", s)
	}
	if p.CallCount("Create") != 1 {
		t.Errorf("sold out did not reach Create (%d)", p.CallCount("Create"))
	}
	if err == nil || strings.Contains(err.Error(), "\n") || !strings.Contains(err.Error(),
		"(the API refused it; no server was created); per Hetzner's listing, cx23 is available in nbg1, hel1 right now; "+
			"the cheapest available in fsn1: cax11 EUR 3.79/mo, cx43 EUR 9.49/mo, ccx13 EUR 12.49/mo; see 'devvm providers types hetzner --location fsn1'") {
		t.Errorf("sold out, refused: %v", err)
	}
	// cx33 is sold out everywhere.
	if err := create("--server-type", "cx33", "--location", "nbg1"); err == nil || !strings.Contains(err.Error(), "per Hetzner's listing, cx33 is sold out everywhere right now") {
		t.Errorf("sold out everywhere: %v", err)
	}

	// The API's own codes: the hint for its "unavailable" answer even where
	// the listing said available (it lags); for another refusal only where
	// the listing said sold out (the code a sold-out Create gets is not
	// pinned down), and never for a name or key one.
	for _, tc := range []struct {
		code, loc string
		hint      bool
	}{{"resource_unavailable", "nbg1", true}, {"placement_error", "nbg1", true}, {"invalid_input", "nbg1", false},
		{"invalid_input", "fsn1", true}, {"uniqueness_error", "fsn1", false}, {"not_found", "fsn1", false}} {
		spy.createErr = &hetzner.APIError{Method: "POST", Path: "/servers", Status: 412, Code: tc.code, Message: "no"}
		err := create("--location", tc.loc)
		if err == nil || !strings.Contains(err.Error(), "the API refused it") || strings.Contains(err.Error(), "listing") != tc.hint {
			t.Errorf("%s in %s: %v (hint %v)", tc.code, tc.loc, err, tc.hint)
		}
	}

	// Accepted (a deprecated type still available counts), and with the
	// catalogue down the API decides, silently.
	spy.createErr = errors.New("stop here")
	for _, argv := range [][]string{
		{"--location", "nbg1"},
		{"--server-type", "cx22", "--location", "nbg1"},
	} {
		if err := create(argv...); err == nil || !strings.Contains(err.Error(), "stop here") || strings.Contains(err.Error(), "listing") {
			t.Errorf("%v: %v, want it to reach Create", argv, err)
		}
		if s := a.Stderr.(*bytes.Buffer).String(); s != "" {
			t.Errorf("%v: stderr %q", argv, s)
		}
	}
	p.CatalogErr = errors.New("dial tcp: no route to host")
	if err := create(); err == nil || !strings.Contains(err.Error(), "stop here") {
		t.Errorf("catalogue down: %v, want it to reach Create", err)
	}
	if s := a.Stderr.(*bytes.Buffer).String(); s != "" {
		t.Errorf("a non-interactive create spoke of the catalogue: %q", s)
	}
}

// On a terminal: the catalogue's selects when it can be fetched, then the
// check; the text inputs with one stderr line when it cannot; nothing said
// when every field came by flag. Each case sets up its own catalogue.
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
	var pickTried bool
	askPlacementPicks = func(_ *os.File, pn string, _ *createSpec, pl *config.Placement, cat *provider.Catalog) (bool, error) {
		picked++
		if pn != config.BackendHetzner || cat == nil {
			t.Errorf("picks(%q, %v)", pn, cat)
		}
		if pickTo.Location != "" {
			pl.Location, pl.ServerType = pickTo.Location, pickTo.ServerType
			return pickTried, nil // on to the check
		}
		return false, stop
	}
	askPlacementText = func(*os.File, *createSpec, *config.Placement) error { typed++; return stop }
	const downLine = "devvm: cannot offer Hetzner's server types and locations to pick from (hetzner: listing server types: context deadline exceeded); type them instead\n"

	for _, tc := range []struct {
		name          string
		catDown       bool
		spec          createSpec
		pickTo        config.Placement
		pickTried     bool
		picks, texts  int
		wantErr       string // "" means the form's stop
		wantStderr    string
		wantNoFormErr bool
	}{
		{name: "catalogue", picks: 1},
		// A pick is checked like a flag (the picker offers only what is
		// offered there; a flag-given type it filtered by may not be).
		{name: "a pick not offered there", pickTo: config.Placement{Location: "hel1", ServerType: "cx43"}, picks: 1,
			wantErr: "'cx43' is not offered in hel1"},
		// A sold-out pick the picker's confirm settled is not warned of
		// again; one it did not ask about (both by flag) is. Either way
		// it goes on (here to the extras form, which fails on a file).
		{name: "sold out, confirmed in the picker", pickTo: config.Placement{Location: "fsn1", ServerType: "cx23"}, pickTried: true,
			picks: 1, wantErr: "huh"},
		{name: "sold out by flag on a terminal", spec: createSpec{ServerType: "cx23", Location: "fsn1"},
			pickTo: config.Placement{Location: "fsn1", ServerType: "cx23"}, picks: 1, wantErr: "huh",
			wantStderr: "devvm: cx23 is listed as sold out in fsn1; trying anyway\n"},
		{name: "catalogue down", catDown: true, texts: 1, wantStderr: downLine},
		// Type and location by flag: the picker still runs (for the image)
		// and nothing is said of the catalogue, up or down.
		{name: "flags, catalogue up", spec: createSpec{ServerType: "cx23", Location: "nbg1"}, picks: 1},
		{name: "flags, catalogue down", catDown: true, spec: createSpec{ServerType: "cx23", Location: "nbg1"}, texts: 1},
		// --yes: no form at all.
		{name: "--yes", spec: createSpec{Yes: true, Location: "nbg1"}, wantNoFormErr: true},
	} {
		p.CatalogErr = nil
		if tc.catDown {
			p.CatalogErr = errors.New("context deadline exceeded")
		}
		picked, typed, pickTo, pickTried = 0, 0, tc.pickTo, tc.pickTried
		var w bytes.Buffer
		a.Stderr = &w
		s := tc.spec
		s.Name, s.Backend = "web", config.BackendHetzner
		_, _, _, err := a.gatherProviderSpec(&s, config.ProviderSpec{Location: "fsn1"}, p)
		switch {
		case tc.wantNoFormErr:
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
		case tc.wantErr == "":
			if err != stop {
				t.Errorf("%s: err %v, want the form's", tc.name, err)
			}
		case err == nil || !strings.Contains(err.Error(), tc.wantErr):
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.wantErr)
		}
		if picked != tc.picks || typed != tc.texts || w.String() != tc.wantStderr {
			t.Errorf("%s: picked %d typed %d stderr %q, want %d %d %q", tc.name, picked, typed, w.String(), tc.picks, tc.texts, tc.wantStderr)
		}
	}
}

// The picker's content, built as data: every location offering something
// (or offering a flag-given type), with what is available there; types
// there available first, cheapest first, then the sold-out ones labelled
// with where they are available; the seed never a sold-out one.
func TestPickerOptions(t *testing.T) {
	cat := fake.SampleCatalog()
	locs := locationOptions(cat, "")
	if v := optionValues(locs); !slices.Equal(v, []string{"fsn1", "nbg1", "hel1", "ash"}) {
		t.Errorf("locations = %v", v)
	}
	// Aligned columns: "ash" is padded to "fsn1", every city to the longest;
	// the count is of available types only (nbg1 has two sold out too).
	if locs[1].Label != "nbg1  Nuremberg, DE    (3 types, from EUR 3.49/mo)" {
		t.Errorf("nbg1 label = %q", locs[1].Label)
	}
	if locs[3].Label != "ash   Ashburn, VA, US  (1 type, from EUR 13.99/mo)" {
		t.Errorf("ash label = %q", locs[3].Label)
	}
	forCX43 := locationOptions(cat, "cx43")
	if v := optionValues(forCX43); !slices.Equal(v, []string{"fsn1", "nbg1"}) {
		t.Errorf("locations for cx43 = %v (not hel1: not offered there)", v)
	}
	if forCX43[0].Label != "fsn1  Falkenstein, DE  (cx43 EUR 9.49/mo)" || forCX43[0].SoldOut ||
		forCX43[1].Label != "nbg1  Nuremberg, DE    (cx43 EUR 9.49/mo, sold out)" || !forCX43[1].SoldOut {
		t.Errorf("cx43 location labels = %+v", forCX43)
	}
	if len(locationOptions(cat, "nope")) != 0 {
		t.Error("an unknown type has locations")
	}
	if got := seedPick(forCX43, "nbg1"); got != "fsn1" {
		t.Errorf("seed for cx43 in a sold-out nbg1 = %q", got)
	}
	// A location with nothing available now is still listed, said so.
	empty := fake.SampleCatalog()
	empty.Types = []provider.ServerType{{Name: "cx23", CPU: "shared", Arch: "x86", Cores: 2, MemoryGB: 4, DiskGB: 40, SoldOut: []string{"fsn1"}}}
	if l := locationOptions(empty, ""); len(l) != 1 || l[0].Label != "fsn1  Falkenstein, DE  (1 type, all sold out)" || !l[0].SoldOut {
		t.Errorf("nothing available = %+v", l)
	}

	types := typeOptions(cat, "nbg1")
	if v := optionValues(types); !slices.Equal(v, []string{"cx23", "cax11", "ccx13", "cx33", "cx43"}) {
		t.Errorf("types = %v (available cheapest first, then sold out; no deprecated cx22)", v)
	}
	for i, want := range []string{
		"cx23   shared x86     2 cores   4 GB   40 GB   3.49",
		"cx33   shared x86     4 cores   8 GB   80 GB   5.49  sold out", // padded to 12.49
		"cx43   shared x86     8 cores  16 GB  160 GB   9.49  sold out (available: fsn1)",
	} {
		if got := types[[]int{0, 3, 4}[i]]; got.Label != want || got.SoldOut != (i > 0) {
			t.Errorf("label =\n%q (sold out %v)\nwant\n%q", got.Label, got.SoldOut, want)
		}
	}
	one := &provider.Catalog{Currency: "EUR", Types: []provider.ServerType{{Name: "cpx02", CPU: "shared", Arch: "x86", Cores: 1, MemoryGB: 1, DiskGB: 20,
		Available: []string{"hel1"}, Prices: map[string]provider.Price{"hel1": {Monthly: 4.99}}}}}
	if got := typeOptions(one, "hel1")[0].Label; got != "cpx02  shared x86  1 core   1 GB  20 GB  4.99" {
		t.Errorf("one-core label = %q", got)
	}
	if len(typeOptions(cat, "mars")) != 0 {
		t.Error("an unknown location has types")
	}
	if got := typeOptionsTitle(cat, "nbg1"); got != "Server type in nbg1: cores, RAM, disk, EUR/month (available first, cheapest first)" {
		t.Errorf("title = %q", got)
	}
	// A label never wraps a 100-column terminal (huh's select breaks): the
	// widest specs, sold out where they are available in five places, cut
	// the list to fit.
	wide := &provider.Catalog{Currency: "EUR", Types: []provider.ServerType{
		{Name: "ccx63", CPU: "dedicated", Arch: "x86", Cores: 48, MemoryGB: 192, DiskGB: 960, Available: []string{"fsn1"},
			Prices: map[string]provider.Price{"fsn1": {Monthly: 1036.49}}},
		{Name: "cx23", CPU: "shared", Arch: "x86", Cores: 2, MemoryGB: 4, DiskGB: 40, SoldOut: []string{"zz9"},
			Available: []string{"fsn1", "nbg1", "hel1", "ash", "hil", "sin", "lon1"}, Prices: map[string]provider.Price{"zz9": {Monthly: 5.49}}},
	}}
	wide.Types[0].Available = append(wide.Types[0].Available, "zz9")
	wide.Types[0].Prices["zz9"] = provider.Price{Monthly: 1036.49}
	for _, o := range typeOptions(wide, "zz9") {
		if len(o.Label) > pickLabelMax {
			t.Errorf("label %d wide: %q", len(o.Label), o.Label)
		}
		if o.Value == "cx23" && !strings.HasSuffix(o.Label, "sold out (available: fsn1, nbg1 +5)") {
			t.Errorf("cut list: %q", o.Label)
		}
	}
	for _, tc := range []struct {
		locs []string
		room int
		want string
	}{
		{nil, 50, ""},
		{[]string{"fsn1", "hel1"}, 50, " (available: fsn1, hel1)"},
		{[]string{"fsn1", "hel1"}, 23, " (available: fsn1 +1)"},
		{[]string{"fsn1", "hel1"}, 5, ""},
	} {
		if got := availableList(tc.locs, tc.room); got != tc.want {
			t.Errorf("availableList(%v, %d) = %q, want %q", tc.locs, tc.room, got, tc.want)
		}
	}
	for _, tc := range []struct{ want, seed string }{{"ccx13", "ccx13"}, {"cx43", "cx23"}, {"nope", "cx23"}, {"", "cx23"}} {
		if got := seedPick(types, tc.want); got != tc.seed {
			t.Errorf("seedPick(%q) = %q, want %q", tc.want, got, tc.seed)
		}
	}
	if seedPick(nil, "x") != "" {
		t.Error("seedPick of nothing")
	}
	if got := seedPick([]pickOption{{Value: "a", SoldOut: true}}, "a"); got != "a" {
		t.Errorf("all sold out seeds the first: %q", got)
	}

	// The confirm a sold-out choice gets, and the --yes warning.
	for _, tc := range []struct {
		pl       config.Placement
		question string
	}{
		{config.Placement{ServerType: "cx23", Location: "fsn1"}, "cx23 is sold out in fsn1. Try anyway?"},
		{config.Placement{ServerType: "cx23", Location: "nbg1"}, ""},
		{config.Placement{ServerType: "cx23", Location: "ash"}, ""}, // not offered: checkPlacement's
	} {
		if got := soldOutQuestion(cat, tc.pl); got != tc.question {
			t.Errorf("soldOutQuestion(%v) = %q, want %q", tc.pl, got, tc.question)
		}
		if got := soldOutLine(cat, tc.pl); (got != "") != (tc.question != "") {
			t.Errorf("soldOutLine(%v) = %q", tc.pl, got)
		}
	}
}

func optionValues(opts []pickOption) []string {
	out := make([]string, len(opts))
	for i, o := range opts {
		out[i] = o.Value
	}
	return out
}

// Offered, PriceIn and the location sets: the order and the three states
// everything above relies on.
func TestCatalogOrdering(t *testing.T) {
	cat := fake.SampleCatalog()
	names := func(ts []provider.ServerType) []string {
		var out []string
		for _, st := range ts {
			out = append(out, st.Name)
		}
		return out
	}
	if got := names(cat.Offered("")); !slices.Equal(got, []string{"cx23", "cax11", "cx43", "ccx13", "cx33"}) {
		t.Errorf("offered = %v", got)
	}
	if got := names(cat.Offered("fsn1")); !slices.Equal(got, []string{"cax11", "cx43", "ccx13", "cx23", "cx33"}) {
		t.Errorf("offered in fsn1 = %v", got)
	}
	if got := names(cat.AvailableTypes("fsn1")); !slices.Equal(got, []string{"cax11", "cx43", "ccx13"}) {
		t.Errorf("available in fsn1 = %v", got)
	}
	cx43, _ := cat.Type("cx43")
	for loc, want := range map[string]provider.Status{"fsn1": provider.Available, "nbg1": provider.SoldOut, "hel1": provider.NotOffered, "ash": provider.NotOffered} {
		if got := cx43.StatusIn(loc); got != want {
			t.Errorf("cx43 in %s = %v, want %v", loc, got, want)
		}
	}
	ccx, _ := cat.Type("ccx13")
	if p, _ := ccx.PriceIn(""); p.Monthly != 12.49 {
		t.Errorf("ccx13 from %v", p)
	}
	if p, _ := ccx.PriceIn("ash"); p.Monthly != 13.99 {
		t.Errorf("ccx13 in ash = %v", p)
	}
	if cx33, _ := cat.Type("cx33"); func() float64 { p, _ := cx33.PriceIn(""); return p.Monthly }() != 5.49 {
		t.Error("a type sold out everywhere is priced by its sold-out locations")
	}
	if p, ok := (provider.ServerType{}).PriceIn(""); ok || p.Monthly != 0 {
		t.Error("an unpriced type has a price")
	}
	if n := len(cat.OfferingLocations()); n != 4 {
		t.Errorf("%d offering locations", n)
	}
}

// Every line under a table is at most as wide as the table (fixed breaks,
// no terminal-width detection), whatever the VAT and IPv4 lines say.
func TestCatalogNotesFitTheTable(t *testing.T) {
	cat := fake.SampleCatalog()
	for _, vat := range []float64{0, 19} {
		cat.VATRate = vat
		for _, varies := range []bool{false, true} {
			cat.IPv4 = map[string]provider.Price{"fsn1": {Monthly: 0.5, Hourly: 0.0008}}
			if varies {
				cat.IPv4["ash"] = provider.Price{Monthly: 0.6, Hourly: 0.001}
			}
			for name, out := range map[string][]string{"grid": typeGrid(styler{}, "hetzner", cat), "nbg1": typeTable(styler{}, "hetzner", cat, "nbg1")} {
				width, under := 0, false
				for _, l := range out[1:] { // the title is not a note
					if l == "" {
						under = true
						continue
					}
					if !under {
						width = max(width, len(l))
					} else if len(l) > width {
						t.Errorf("%s (vat %v, varies %v): note %q is %d wide, the table %d", name, vat, varies, l, len(l), width)
					}
				}
				if !under {
					t.Errorf("%s: no notes", name)
				}
			}
		}
	}
}

// pickerScript answers askPlacementPicks' prompts in order: each select
// answer is a value (checked against the options offered), each confirm a
// bool; it records the options of every select.
type pickerScript struct {
	t        *testing.T
	selects  []string
	confirms []bool
	seen     [][]pickOption
	asked    []string
}

func (p *pickerScript) install(t *testing.T) {
	oldSel, oldConf := pickSelect, pickConfirm
	t.Cleanup(func() { pickSelect, pickConfirm = oldSel, oldConf })
	pickSelect = func(_ *os.File, title string, opts []pickOption, _ int, v *string) error {
		p.seen = append(p.seen, opts)
		if len(p.selects) == 0 {
			return errors.New("script ran out: " + title)
		}
		*v, p.selects = p.selects[0], p.selects[1:]
		if !slices.Contains(optionValues(opts), *v) {
			p.t.Errorf("%s: answered %q, not an option (%v)", title, *v, optionValues(opts))
		}
		return nil
	}
	pickConfirm = func(_ *os.File, q string) (bool, error) {
		p.asked = append(p.asked, q)
		if len(p.confirms) == 0 {
			return false, errors.New("script ran out: " + q)
		}
		yes := p.confirms[0]
		p.confirms = p.confirms[1:]
		return yes, nil
	}
}

// The picker's loop: with the location picked, the type select ends in
// "← another location", which returns to the location select (seeded
// again, never on itself); a sold-out type asks, No picks again, Yes is
// returned as tried; with the location by flag, the grid is still shown
// and there is no way back.
func TestPickerBackAndConfirm(t *testing.T) {
	cat := fake.SampleCatalog()
	tty, err := os.CreateTemp(t.TempDir(), "tty")
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()

	// ash has only ccx13: back, then fsn1's cx23 (sold out there): No,
	// cx23 again: Yes.
	sc := &pickerScript{t: t, selects: []string{"ash", pickBack, "fsn1", "cx23", "cx23"}, confirms: []bool{false, true}}
	sc.install(t)
	s := &createSpec{Image: "ubuntu-24.04"}
	pl := config.Placement{Location: "fsn1", ServerType: "cx23", Image: "ubuntu-24.04"}
	tried, err := askPlacementPicks(tty, "hetzner", s, &pl, cat)
	if err != nil || !tried || pl.Location != "fsn1" || pl.ServerType != "cx23" {
		t.Fatalf("picks = %v, %v, %+v", tried, err, pl)
	}
	if len(sc.seen) != 5 || len(sc.asked) != 2 || sc.asked[0] != "cx23 is sold out in fsn1. Try anyway?" {
		t.Errorf("selects %d, confirms %v", len(sc.seen), sc.asked)
	}
	for _, i := range []int{1, 3, 4} { // every type select ends in the way back
		last := sc.seen[i][len(sc.seen[i])-1]
		if !last.Back || last.Value != pickBack || last.Label != "← another location" {
			t.Errorf("type select %d ends in %+v", i, last)
		}
	}
	data, _ := os.ReadFile(tty.Name())
	if !strings.HasPrefix(string(data), "Hetzner server types right now") || strings.Count(string(data), "NAME") != 1 {
		t.Errorf("picture = %q (the grid, once)", data)
	}

	// An available type after a back: not tried.
	sc = &pickerScript{t: t, selects: []string{"fsn1", pickBack, "nbg1", "cx23"}}
	sc.install(t)
	pl = config.Placement{Location: "fsn1", ServerType: "cx23", Image: "x"}
	if tried, err := askPlacementPicks(tty, "hetzner", s, &pl, cat); err != nil || tried || pl != (config.Placement{Location: "nbg1", ServerType: "cx23", Image: "x"}) {
		t.Errorf("after a back: %v, %v, %+v", tried, err, pl)
	}

	// Location by flag: the grid, no way back.
	t2, _ := os.CreateTemp(t.TempDir(), "tty")
	defer t2.Close()
	sc = &pickerScript{t: t, selects: []string{"ccx13"}}
	sc.install(t)
	pl = config.Placement{Location: "nbg1", ServerType: "cx23", Image: "x"}
	if tried, err := askPlacementPicks(t2, "hetzner", &createSpec{Location: "nbg1", Image: "x"}, &pl, cat); err != nil || tried || pl.ServerType != "ccx13" {
		t.Errorf("location by flag: %v, %v, %+v", tried, err, pl)
	}
	if slices.Contains(optionValues(sc.seen[0]), pickBack) {
		t.Error("a way back with the location by flag")
	}
	if data, _ := os.ReadFile(t2.Name()); !strings.HasPrefix(string(data), "Hetzner server types right now") {
		t.Errorf("picture = %q, want the grid", data)
	}

	// The type by flag: the location confirms, Yes is tried, no type select.
	sc = &pickerScript{t: t, selects: []string{"nbg1"}, confirms: []bool{true}}
	sc.install(t)
	pl = config.Placement{Location: "fsn1", ServerType: "cx43", Image: "x"}
	if tried, err := askPlacementPicks(tty, "hetzner", &createSpec{ServerType: "cx43", Image: "x"}, &pl, cat); err != nil || !tried || pl.Location != "nbg1" || len(sc.seen) != 1 {
		t.Errorf("type by flag: %v, %v, %+v", tried, err, pl)
	}

	// Nothing to pick: a refusal, no select, no image prompt, whatever
	// checkPlacement says of the seed (a deprecated cx22 it would pass).
	onlyOld := fake.SampleCatalog()
	onlyOld.Types = onlyOld.Types[5:] // cx22, deprecated, in nbg1
	for name, tc := range map[string]struct {
		cat  *provider.Catalog
		spec createSpec
		want string
	}{
		"empty catalogue":         {&provider.Catalog{}, createSpec{}, "has no location 'nbg1'"},
		"location by flag, empty": {onlyOld, createSpec{Location: "nbg1"}, "lists nothing to pick from"},
		"deprecated only":         {onlyOld, createSpec{}, "lists nothing to pick from"},
		"type by flag, nowhere":   {cat, createSpec{ServerType: "cx99"}, "no server type 'cx99'"},
	} {
		sc = &pickerScript{t: t}
		sc.install(t)
		pl = config.Placement{Location: "nbg1", ServerType: "cx22"}
		if tc.spec.ServerType != "" {
			pl.ServerType = tc.spec.ServerType
		}
		if _, err := askPlacementPicks(tty, "hetzner", &tc.spec, &pl, tc.cat); err == nil || !strings.Contains(err.Error(), tc.want) || len(sc.seen) != 0 {
			t.Errorf("%s: %v (selects %d), want %q", name, err, len(sc.seen), tc.want)
		}
	}

	// Widths in runes, as fmt pads: the widest city, not ASCII, lines up.
	accented := fake.SampleCatalog()
	accented.Locations[1].City = "Nürnberg-Fürth"
	for _, o := range locationOptions(accented, "") {
		if i := strings.Index(o.Label, "("); utf8.RuneCountInString(o.Label[:i]) != 26 {
			t.Errorf("misaligned: %q", o.Label)
		}
	}

	// The seed is never the way back, even with every type sold out.
	allGone := []pickOption{{Value: "cx33", SoldOut: true}, backOption()}
	if got := seedPick(allGone, pickBack); got != "cx33" {
		t.Errorf("seed = %q", got)
	}
	if got := seedPick([]pickOption{backOption()}, ""); got != "" {
		t.Errorf("seed of only the way back = %q", got)
	}
}

// Through create's own path: a sold-out pick the picker's confirm settled
// gets no stderr line (the --yes warning is for no prompt only).
func TestPickerConfirmNoWarning(t *testing.T) {
	a := newTestApp(t)
	p := withCatalog(t, a, "")
	tty, err := os.CreateTemp(t.TempDir(), "tty")
	if err != nil {
		t.Fatal(err)
	}
	oldTTY := openTTY
	t.Cleanup(func() { openTTY = oldTTY })
	openTTY = func() *os.File { f, _ := os.Open(tty.Name()); return f }
	sc := &pickerScript{t: t, selects: []string{"fsn1", "cx23"}, confirms: []bool{true}}
	sc.install(t)
	var w bytes.Buffer
	a.Stderr = &w
	s := createSpec{Name: "web", Backend: config.BackendHetzner, Image: "ubuntu-24.04"}
	// The hook form after the picks fails on a file: far enough.
	_, _, _, err = a.gatherProviderSpec(&s, config.ProviderSpec{Location: "fsn1"}, p)
	if err == nil || !strings.Contains(err.Error(), "huh") || len(sc.asked) != 1 {
		t.Fatalf("err %v, asked %v", err, sc.asked)
	}
	if w.String() != "" {
		t.Errorf("stderr after a picker confirm = %q", w.String())
	}
}
