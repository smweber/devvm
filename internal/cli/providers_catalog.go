package cli

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/provider"
)

// The provider's catalogue (providers.md §9): what server types it sells,
// their specs and prices, and where each can be made now. One live fetch
// per command (the provider.Cataloger capability), never cached: it changes
// as types sell out, and only a human's command asks for it. Three uses:
// `providers types` lists it; `create --backend P` on a terminal picks the
// location and type from it; and every create refuses, before Create, a
// type the catalogue says does not exist or is not available in the
// location. The catalogue is never why a create fails to happen: if it
// cannot be fetched, the picker falls back to the text inputs (one stderr
// line) and the check is skipped, leaving the API to decide as before.

// fetchCatalog is the client's catalogue under providerTimes.catalog.
func fetchCatalog(pn string, cl provider.Provider) (*provider.Catalog, error) {
	cg, ok := cl.(provider.Cataloger)
	if !ok {
		return nil, fmt.Errorf("%s: this client cannot list server types", pn)
	}
	ctx, cancel := context.WithTimeout(context.Background(), providerTimes.catalog)
	defer cancel()
	cat, err := cg.Catalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: listing server types: %w", pn, err)
	}
	return cat, nil
}

func (a *App) providersTypesCmd() *cobra.Command {
	var loc string
	var plain bool
	c := &cobra.Command{
		Use:   "types PROVIDER [--location LOC] [--plain]",
		Short: "List the server types a provider sells: specs, prices, where available",
		Long: "One live, read-only listing of the provider's server types, cheapest first:\n" +
			"CPU kind and architecture, cores, RAM, disk, price per month and per hour,\n" +
			"and the locations a server of the type can be made in now. Deprecated types\n" +
			"are left out. --location keeps the types available there, priced there;\n" +
			"without it a price is the lowest among the type's locations. Prices include\n" +
			"VAT at the account's rate; a public IPv4 address is billed on top (both said\n" +
			"under the table). Needs this host's API token (devvm providers token).\n\n" +
			"--plain prints one tab-separated line per type, no header:\n" +
			"  name  cpu  arch  cores  memory_gb  disk_gb  monthly  hourly  currency  locations\n" +
			"cpu is shared|dedicated, arch x86|arm; monthly and hourly are decimals, VAT\n" +
			"included as above; locations is comma-separated, '-' when none.",
		Args:      cobra.ExactArgs(1),
		ValidArgs: config.KnownProviders(),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runProvidersTypes(args[0], strings.TrimSpace(loc), plain)
		},
	}
	c.Flags().StringVar(&loc, "location", "", "only the types available in LOC, priced there")
	c.Flags().BoolVar(&plain, "plain", false, "machine-readable: one tab-separated line per type (columns in --help)")
	return c
}

func (a *App) runProvidersTypes(pn, loc string, plain bool) error {
	if !slices.Contains(config.KnownProviders(), pn) {
		return fmt.Errorf("unknown provider %q (devvm knows: %s)", pn, strings.Join(config.KnownProviders(), ", "))
	}
	cl, err := a.providerClient(pn)
	if err != nil {
		return err
	}
	cat, err := fetchCatalog(pn, cl)
	if err != nil {
		return err
	}
	if loc != "" {
		if _, ok := cat.Location(loc); !ok {
			return fmt.Errorf("%s has no location '%s' (it has: %s)", providerTitle(pn), loc, locationNames(cat))
		}
	}
	types := cat.Offered(loc)
	if plain {
		for _, t := range types {
			fmt.Fprintln(a.Stdout, plainTypeRow(cat, t, loc))
		}
		return nil
	}
	a.printTypes(pn, cat, types, loc)
	return nil
}

// plainTypeRow is one --plain line (a compatibility surface once shipped:
// append columns, never reorder).
func plainTypeRow(cat *provider.Catalog, t provider.ServerType, loc string) string {
	p, _ := t.PriceIn(loc)
	locs := strings.Join(t.Available, ",")
	if locs == "" {
		locs = "-"
	}
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	return strings.Join([]string{t.Name, t.CPU, t.Arch, strconv.Itoa(t.Cores), f(t.MemoryGB), strconv.Itoa(t.DiskGB),
		f(p.Monthly), f(p.Hourly), cat.Currency, locs}, "\t")
}

// printTypes is the human table, then what the prices mean.
func (a *App) printTypes(pn string, cat *provider.Catalog, types []provider.ServerType, loc string) {
	where := ""
	if loc != "" {
		where = " in " + locationLabel(cat, loc)
	}
	if len(types) == 0 {
		fmt.Fprintf(a.Stdout, "No %s server type is available%s right now.\n", providerTitle(pn), where)
		return
	}
	fmt.Fprintf(a.Stdout, "%s server types%s, cheapest first:\n", providerTitle(pn), where)
	w := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	cur := cat.Currency
	fmt.Fprintf(w, "NAME\tCPU\tCORES\tRAM\tDISK\t%s/MONTH\t%s/HOUR\tLOCATIONS\n", cur, cur)
	varies, nowhere := false, false
	for _, t := range types {
		p, _ := t.PriceIn(loc)
		mark := ""
		if loc == "" && t.PriceVaries() {
			mark, varies = "*", true
		}
		locs := strings.Join(t.Available, " ")
		if locs == "" {
			locs, nowhere = "-", true
		}
		fmt.Fprintf(w, "%s\t%s %s\t%d\t%s GB\t%d GB\t%.2f%s\t%.4f\t%s\n",
			t.Name, t.CPU, t.Arch, t.Cores, gb(t.MemoryGB), t.DiskGB, p.Monthly, mark, p.Hourly, locs)
	}
	w.Flush()
	fmt.Fprintln(a.Stdout)
	if varies {
		fmt.Fprintf(a.Stdout, "* the lowest of the type's locations; it costs more in others (--location LOC prices one).\n")
	}
	if nowhere {
		fmt.Fprintf(a.Stdout, "- not available in any location right now (sold out or phased out).\n")
	}
	for _, l := range priceNotes(pn, cat, loc) {
		fmt.Fprintln(a.Stdout, l)
	}
}

// priceNotes says what the prices include: the account's VAT (the API's
// gross prices, at the rate /pricing reports for this account), the
// hourly billing, and the IPv4 address the type's price leaves out.
func priceNotes(pn string, cat *provider.Catalog, loc string) []string {
	vat := fmt.Sprintf("Prices include VAT at this account's rate (%s%%).", strconv.FormatFloat(cat.VATRate, 'f', -1, 64))
	if cat.VATRate == 0 {
		vat = "This account is charged no VAT (rate 0%), so the prices are net and gross alike."
	}
	out := []string{vat + " " + providerTitle(pn) + " bills by the hour, up to the monthly price, and a stopped server still bills."}
	if ip, ok := ipv4Price(cat, loc); ok {
		out = append(out, fmt.Sprintf("A public IPv4 address is billed on top: %s/month (%s/hour)%s; ipv4 = false in [providers.%s] or a template makes an IPv6-only server without it.",
			money(cat.Currency, ip.Monthly), strconv.FormatFloat(ip.Hourly, 'f', -1, 64), ipv4Where(cat, loc), pn))
	}
	return out
}

// ipv4Price is the IPv4 price in loc, or for "" the one price every
// location shares (the highest, if they differ, so the note never
// understates it).
func ipv4Price(cat *provider.Catalog, loc string) (provider.Price, bool) {
	if loc != "" {
		p, ok := cat.IPv4[loc]
		return p, ok
	}
	var top provider.Price
	for _, p := range cat.IPv4 {
		if p.Monthly > top.Monthly {
			top = p
		}
	}
	return top, len(cat.IPv4) > 0
}

func ipv4Where(cat *provider.Catalog, loc string) string {
	if loc != "" {
		return " in " + loc
	}
	first, varies := -1.0, false
	for _, p := range cat.IPv4 {
		if first >= 0 && p.Monthly != first {
			varies = true
		}
		first = p.Monthly
	}
	if varies {
		return " at most, depending on the location"
	}
	return ""
}

func gb(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func money(cur string, v float64) string { return fmt.Sprintf("%s %.2f", cur, v) }

func locationNames(cat *provider.Catalog) string {
	names := make([]string, len(cat.Locations))
	for i, l := range cat.Locations {
		names[i] = l.Name
	}
	return strings.Join(names, ", ")
}

// locationLabel is "nbg1 (Nuremberg, DE)".
func locationLabel(cat *provider.Catalog, loc string) string {
	if l, ok := cat.Location(loc); ok && l.City != "" {
		return fmt.Sprintf("%s (%s, %s)", l.Name, l.City, l.Country)
	}
	return loc
}

// cheapestIn is the few cheapest types available in loc, for a refusal.
func cheapestIn(cat *provider.Catalog, loc string, n int) string {
	var out []string
	for _, t := range cat.Offered(loc) {
		if len(out) == n {
			break
		}
		p, _ := t.PriceIn(loc)
		out = append(out, fmt.Sprintf("%s %s/mo", t.Name, money(cat.Currency, p.Monthly)))
	}
	return strings.Join(out, ", ")
}

// checkPlacement refuses, before Create, a placement the catalogue says
// the API would refuse: a location it does not have, a type it does not
// sell, or one not available in the location (sold out or phased out
// there). A deprecated type still available passes: the API may take it.
// The image is left to the API: names resolve per architecture there.
func checkPlacement(pn string, cat *provider.Catalog, pl config.Placement) error {
	title := providerTitle(pn)
	if _, ok := cat.Location(pl.Location); !ok {
		return fmt.Errorf("%s has no location '%s' (it has: %s); 'devvm providers types %s' lists what each offers",
			title, pl.Location, locationNames(cat), pn)
	}
	t, known := cat.Type(pl.ServerType)
	if known && t.AvailableIn(pl.Location) {
		return nil
	}
	why := fmt.Sprintf("%s has no server type '%s'", title, pl.ServerType)
	if known {
		elsewhere := "it is not available anywhere right now"
		if len(t.Available) > 0 {
			elsewhere = "it is available in " + strings.Join(t.Available, ", ")
		}
		why = fmt.Sprintf("server type '%s' is not available in %s (%s)", pl.ServerType, pl.Location, elsewhere)
	}
	there := "nothing is available there right now"
	if c := cheapestIn(cat, pl.Location, 3); c != "" {
		there = "the cheapest there: " + c
	}
	return fmt.Errorf("%s; %s; see 'devvm providers types %s --location %s'", why, there, pn, pl.Location)
}

// pickOption is one select option, built as data so the picker's content
// is tested without driving huh.
type pickOption struct{ Label, Value string }

// locationOptions is the location picker: every location where something
// offered is available (where typeName is, when the type came by flag),
// labelled with its city and the cheapest type there.
func locationOptions(cat *provider.Catalog, typeName string) []pickOption {
	var out []pickOption
	locs := cat.OfferedLocations(typeName)
	place := func(l provider.Location) string {
		if l.City == "" {
			return ""
		}
		return l.City + ", " + l.Country
	}
	nameW, placeW := 0, 0 // aligned columns, as typeOptions ("ash" beside "fsn1")
	for _, l := range locs {
		nameW, placeW = max(nameW, len(l.Name)), max(placeW, len(place(l)))
	}
	for _, l := range locs {
		label := fmt.Sprintf("%-*s", nameW, l.Name)
		if placeW > 0 {
			label += fmt.Sprintf("  %-*s", placeW, place(l))
		}
		if typeName != "" {
			t, _ := cat.Type(typeName)
			p, _ := t.PriceIn(l.Name)
			label += fmt.Sprintf("  (%s %s/mo)", typeName, money(cat.Currency, p.Monthly))
		} else {
			offered := cat.Offered(l.Name)
			p, _ := offered[0].PriceIn(l.Name)
			label += fmt.Sprintf("  (%d types, from %s/mo)", len(offered), money(cat.Currency, p.Monthly))
		}
		out = append(out, pickOption{Label: label, Value: l.Name})
	}
	return out
}

// typeOptions is the type picker for loc: the offered types available
// there, cheapest first, each with its specs and monthly price there,
// in aligned columns.
func typeOptions(cat *provider.Catalog, loc string) []pickOption {
	types := cat.Offered(loc)
	nameW, cpuW := 0, 0
	for _, t := range types {
		nameW, cpuW = max(nameW, len(t.Name)), max(cpuW, len(t.CPU)+1+len(t.Arch))
	}
	out := make([]pickOption, len(types))
	for i, t := range types {
		p, _ := t.PriceIn(loc)
		cores := "cores"
		if t.Cores == 1 {
			cores = "core " // the column stays aligned
		}
		out[i] = pickOption{Value: t.Name, Label: fmt.Sprintf("%-*s  %-*s  %2d %s  %4s GB RAM  %4d GB disk  %s/mo",
			nameW, t.Name, cpuW, t.CPU+" "+t.Arch, t.Cores, cores, gb(t.MemoryGB), t.DiskGB, money(cat.Currency, p.Monthly))}
	}
	return out
}

// seedPick is the option a select starts on: want when it is offered,
// else the first (the cheapest, for types).
func seedPick(opts []pickOption, want string) string {
	for _, o := range opts {
		if o.Value == want {
			return want
		}
	}
	if len(opts) == 0 {
		return ""
	}
	return opts[0].Value
}

// askPlacementPicks is create's placement form with a catalogue: the
// location, then a type available there, as selects (each only when no
// flag gave it), then the image as text. A list that comes up empty (a
// flag naming a type or location with nothing to offer) is checkPlacement's
// refusal. A var so a test can answer it.
var askPlacementPicks = func(tty *os.File, pn string, s *createSpec, pl *config.Placement, cat *provider.Catalog) error {
	if s.Location == "" {
		opts := locationOptions(cat, s.ServerType)
		if len(opts) == 0 {
			return checkPlacement(pn, cat, *pl)
		}
		pl.Location = seedPick(opts, pl.Location)
		if err := form(tty, huh.NewSelect[string]().Title("Location").Options(huhOptions(opts)...).Value(&pl.Location)); err != nil {
			return err
		}
	}
	if s.ServerType == "" {
		opts := typeOptions(cat, pl.Location)
		if len(opts) == 0 {
			return checkPlacement(pn, cat, *pl)
		}
		pl.ServerType = seedPick(opts, pl.ServerType)
		if err := form(tty, huh.NewSelect[string]().Title("Server type in "+pl.Location+" (cheapest first)").
			Options(huhOptions(opts)...).Value(&pl.ServerType).Height(min(len(opts), 12)+2)); err != nil {
			return err
		}
	}
	if s.Image == "" {
		if err := form(tty, huh.NewInput().Title("Image").Value(&pl.Image).
			Validate(func(v string) error { return config.ValidPlacement("image", strings.TrimSpace(v)) })); err != nil {
			return err
		}
		if pl.Image = strings.TrimSpace(pl.Image); pl.Image == "" {
			return fmt.Errorf("an image is required")
		}
	}
	return nil
}

func huhOptions(opts []pickOption) []huh.Option[string] {
	out := make([]huh.Option[string], len(opts))
	for i, o := range opts {
		out[i] = huh.NewOption(o.Label, o.Value)
	}
	return out
}
