package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/provider"
)

// The provider's catalogue (providers.md §9): what server types it sells,
// their specs and prices, and in each location whether a type is
// available now, sold out now, or not offered. One live fetch per command
// (the provider.Cataloger capability), never cached: availability moves
// within the hour, and only a human's command asks for it. Three uses:
// `providers types` lists it; `create --backend P` on a terminal shows it
// and picks the location and type from it; and every create refuses,
// before Create, a type or location the catalogue does not have or a type
// not offered in the location. Sold out is only a warning: the flag is an
// indicator that lags, so the API decides. The catalogue is never why a
// create fails to happen: if it cannot be fetched, the picker falls back
// to the text inputs (one stderr line) and the check is skipped.

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
		Short: "List the server types a provider sells: specs, prices, where sold out right now",
		Long: "One live, read-only listing of the provider's server types: CPU kind and\n" +
			"architecture, cores, RAM, disk, and in each location the monthly price and\n" +
			"whether a server of the type can be made there right now. Without --location\n" +
			"it is one table, a column per location: '5.49' available, '(5.49)' sold out\n" +
			"there right now (dimmed on a terminal), '-' not offered there. Types available\n" +
			"somewhere come first, cheapest first. --location LOC lists the types offered\n" +
			"there with monthly and hourly prices, a STATUS (available or sold out) and\n" +
			"where each is available instead. Availability changes within the hour.\n" +
			"Deprecated types are left out. Prices include VAT at the account's rate; a\n" +
			"public IPv4 address is billed on top (both said under the table). Needs this\n" +
			"host's API token (devvm providers token).\n\n" +
			"--plain prints one tab-separated line per type and location it is offered in,\n" +
			"no header (not offered: no line; --location: that location's lines only):\n" +
			"  name  cpu  arch  cores  memory_gb  disk_gb  location  status  monthly  hourly  currency\n" +
			"cpu is shared|dedicated, arch x86|arm, status available|soldout; monthly and\n" +
			"hourly are decimals, VAT included as above ('-' when unpriced).",
		Args:      cobra.ExactArgs(1),
		ValidArgs: config.KnownProviders(),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runProvidersTypes(args[0], strings.TrimSpace(loc), plain)
		},
	}
	c.Flags().StringVar(&loc, "location", "", "the types offered in LOC: hourly prices, available or sold out")
	c.Flags().BoolVar(&plain, "plain", false, "machine-readable: one tab-separated line per type and location (columns in --help)")
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
	var lines []string
	switch {
	case plain:
		lines = plainTypeRows(cat, loc)
	case loc != "":
		lines = typeTable(styleFor(a.Stdout), pn, cat, loc)
	default:
		lines = typeGrid(styleFor(a.Stdout), pn, cat)
	}
	for _, l := range lines {
		fmt.Fprintln(a.Stdout, l)
	}
	return nil
}

// plainTypeRows is --plain: a line per type and location offering it, in
// the listing's type order and the provider's location order (a
// compatibility surface once shipped: append columns, never reorder).
func plainTypeRows(cat *provider.Catalog, loc string) []string {
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	var out []string
	for _, t := range cat.Offered(loc) {
		for _, l := range cat.Locations {
			if loc != "" && l.Name != loc {
				continue
			}
			status := "available"
			switch t.StatusIn(l.Name) {
			case provider.NotOffered:
				continue
			case provider.SoldOut:
				status = "soldout"
			}
			monthly, hourly := "-", "-"
			if p, ok := t.Prices[l.Name]; ok {
				monthly, hourly = f(p.Monthly), f(p.Hourly)
			}
			out = append(out, strings.Join([]string{t.Name, t.CPU, t.Arch, strconv.Itoa(t.Cores), f(t.MemoryGB), strconv.Itoa(t.DiskGB),
				l.Name, status, monthly, hourly, cat.Currency}, "\t"))
		}
	}
	return out
}

// specCells are a type's NAME, CPU, CORES, RAM and DISK: the name bold,
// the whole row faint when nothing of it can be had.
func specCells(t provider.ServerType, gone bool) []cell {
	name, rest := bold, plain
	if gone {
		name, rest = faint, faint
	}
	return []cell{{text: t.Name, a: name}, {text: t.CPU + " " + t.Arch, a: rest}, {text: strconv.Itoa(t.Cores), a: rest},
		{text: gb(t.MemoryGB) + " GB", a: rest}, {text: strconv.Itoa(t.DiskGB) + " GB", a: rest}}
}

// specHeader is specCells' header, bold, with more columns after it.
func specHeader(more ...string) []cell {
	var out []cell
	for _, h := range append([]string{"NAME", "CPU", "CORES", "RAM", "DISK"}, more...) {
		out = append(out, cell{text: h, a: bold})
	}
	return out
}

// typeGrid is the listing across locations (and create's picture before
// its picker): a row per offered type, available somewhere first, then a
// column per location with the monthly price there: "5.49" available
// (the row's cheapest in the accent), "(5.49)" sold out right now (faint),
// "-" not offered (faint). Prices are right-aligned on their digits.
func typeGrid(st styler, pn string, cat *provider.Catalog) []string {
	types := cat.Offered("")
	if len(types) == 0 {
		return []string{fmt.Sprintf("%s offers no server type right now.", providerTitle(pn))}
	}
	locs := cat.OfferingLocations()
	var names []string
	for _, l := range locs {
		names = append(names, l.Name)
	}
	right := []bool{false, false, true, true, true}
	header := specHeader(names...)
	for i := range locs {
		right = append(right, true)
		header[5+i].trail = 1
	}
	rows := [][]cell{header}
	for _, t := range types {
		row := specCells(t, len(t.Available) == 0)
		low, any := 0.0, false
		for _, l := range t.Available {
			if p, ok := t.Prices[l]; ok && (!any || p.Monthly < low) {
				low, any = p.Monthly, true
			}
		}
		for _, l := range locs {
			p, priced := t.Prices[l.Name]
			price := "?"
			if priced {
				price = fmt.Sprintf("%.2f", p.Monthly)
			}
			switch t.StatusIn(l.Name) {
			case provider.Available:
				c := cell{text: price, trail: 1}
				if priced && p.Monthly == low {
					c.a = accent
				}
				row = append(row, c)
			case provider.SoldOut:
				row = append(row, cell{text: "(" + price + ")", a: faint})
			default:
				row = append(row, cell{text: "-", a: faint, trail: 1})
			}
		}
		rows = append(rows, row)
	}
	out := []string{st.paint(bold, fmt.Sprintf("%s server types right now, available first, cheapest first:", providerTitle(pn)))}
	out = append(out, renderTable(st, right, rows)...)
	out = append(out, "")
	// The notes are short lines broken by hand, each well inside the grid's
	// width (about 96 columns with Hetzner's six locations), so none wraps
	// mid-word on a terminal the grid fits; no terminal-width detection.
	notes := []string{fmt.Sprintf("%s/month. (price) = maybe sold out, - = not offered.", cat.Currency),
		fmt.Sprintf("Hourly prices: 'devvm providers types %s --location LOC'.", pn)}
	for _, l := range append(notes, priceNotes(cat, "")...) {
		out = append(out, st.paint(faint, l))
	}
	return out
}

// typeTable is --location LOC: the types offered there, available first,
// with monthly and hourly prices there, a STATUS (available, in the
// accent; sold out, the row faint) and where each is available now.
func typeTable(st styler, pn string, cat *provider.Catalog, loc string) []string {
	types := cat.Offered(loc)
	where := locationLabel(cat, loc)
	if len(types) == 0 {
		return []string{fmt.Sprintf("%s offers no server type in %s.", providerTitle(pn), where)}
	}
	cur := cat.Currency
	rows := [][]cell{specHeader(cur+"/MONTH", cur+"/HOUR", "STATUS", "AVAILABLE IN")}
	right := []bool{false, false, true, true, true, true, true}
	for _, t := range types {
		gone := !t.AvailableIn(loc)
		row := specCells(t, gone)
		rest, status := plain, cell{text: "available", a: accent}
		if gone {
			rest, status = faint, cell{text: "sold out", a: faint}
		}
		month, hour := "?", "?"
		if p, ok := t.Prices[loc]; ok {
			month, hour = fmt.Sprintf("%.2f", p.Monthly), fmt.Sprintf("%.4f", p.Hourly)
		}
		avail := strings.Join(t.Available, " ")
		if avail == "" {
			avail = "-"
		}
		rows = append(rows, append(row, cell{text: month, a: rest}, cell{text: hour, a: rest}, status, cell{text: avail, a: rest}))
	}
	out := []string{st.paint(bold, fmt.Sprintf("%s server types offered in %s right now, available first, cheapest first:", providerTitle(pn), where))}
	out = append(out, renderTable(st, right, rows)...)
	out = append(out, "")
	for _, l := range priceNotes(cat, loc) {
		out = append(out, st.paint(faint, l))
	}
	return out
}

// priceNotes says what the prices include, in two short lines broken by
// hand (as typeGrid's notes): the account's VAT (the API's gross prices,
// at the rate /pricing reports for this account), the hourly billing, and
// the IPv4 address the type's price leaves out.
func priceNotes(cat *provider.Catalog, loc string) []string {
	vat := fmt.Sprintf("VAT included (%s%%).", strconv.FormatFloat(cat.VATRate, 'f', -1, 64))
	if cat.VATRate == 0 {
		vat = "No VAT on this account (0%)."
	}
	second := "A stopped server still bills."
	if ip, ok := ipv4Price(cat, loc); ok {
		switch {
		case loc != "":
			second += fmt.Sprintf(" Public IPv4 in %s: %s/month extra.", loc, money(cat.Currency, ip.Monthly))
		case ipv4Varies(cat):
			second += fmt.Sprintf(" Public IPv4: up to %s/month extra.", money(cat.Currency, ip.Monthly))
		default:
			second += fmt.Sprintf(" Public IPv4: %s/month extra.", money(cat.Currency, ip.Monthly))
		}
	}
	return []string{vat + " Billed hourly, up to the monthly price.", second}
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

// ipv4Varies is whether the locations' IPv4 prices differ.
func ipv4Varies(cat *provider.Catalog) bool {
	first := -1.0
	for _, p := range cat.IPv4 {
		if first >= 0 && p.Monthly != first {
			return true
		}
		first = p.Monthly
	}
	return false
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
	for _, t := range cat.AvailableTypes(loc) {
		if len(out) == n {
			break
		}
		p, _ := t.PriceIn(loc)
		out = append(out, fmt.Sprintf("%s %s/mo", t.Name, money(cat.Currency, p.Monthly)))
	}
	return strings.Join(out, ", ")
}

// availableElsewhere is where a type can be had now, for a refusal or a
// hint: "is available in nbg1, hel1 right now".
func availableElsewhere(t provider.ServerType) string {
	switch {
	case len(t.Available) > 0:
		return "is available in " + strings.Join(t.Available, ", ") + " right now"
	case len(t.SoldOut) > 0:
		return "is sold out everywhere right now"
	}
	return "is offered nowhere"
}

// checkPlacement refuses, before Create, a placement the API would refuse
// for good: a location it does not have, a type it does not sell, or one
// not offered in the location. A type sold out there passes (soldOutLine
// warns): the flag lags, and the API decides. It is also plainly wrong at
// times: on 2026-11-24 a cx23 in nbg1 was created while listed sold out
// there, and stayed listed so afterwards (providers.md §9). A deprecated type still
// offered passes too. The image is left to the API: names resolve per
// architecture there.
func checkPlacement(pn string, cat *provider.Catalog, pl config.Placement) error {
	title := providerTitle(pn)
	if _, ok := cat.Location(pl.Location); !ok {
		return fmt.Errorf("%s has no location '%s' (it has: %s); 'devvm providers types %s' lists what each offers",
			title, pl.Location, locationNames(cat), pn)
	}
	t, known := cat.Type(pl.ServerType)
	if known && t.StatusIn(pl.Location) != provider.NotOffered {
		return nil
	}
	why := fmt.Sprintf("%s has no server type '%s'", title, pl.ServerType)
	if known {
		why = fmt.Sprintf("server type '%s' is not offered in %s (it %s)", pl.ServerType, pl.Location, availableElsewhere(t))
	}
	there := "nothing is available there right now"
	if c := cheapestIn(cat, pl.Location, 3); c != "" {
		there = "the cheapest available there: " + c
	}
	return fmt.Errorf("%s; %s; see 'devvm providers types %s --location %s'", why, there, pn, pl.Location)
}

// soldOutLine is create's one warning for a placement the catalogue lists
// as sold out, or "": for --yes or no terminal only (a picker confirm
// already said it).
func soldOutLine(cat *provider.Catalog, pl config.Placement) string {
	if t, ok := cat.Type(pl.ServerType); !ok || t.StatusIn(pl.Location) != provider.SoldOut {
		return ""
	}
	return fmt.Sprintf("devvm: %s is listed as sold out in %s; trying anyway", pl.ServerType, pl.Location)
}

// unavailableHint is what a refused Create gets appended when the refusal
// is the provider's "not available" (or the catalogue said sold out and
// the refusal is not about the name or key): where the type is available
// now and the cheapest available in the location. "" otherwise.
func unavailableHint(pn string, cat *provider.Catalog, pl config.Placement, err error) string {
	if cat == nil || errors.Is(err, provider.ErrConflict) || errors.Is(err, provider.ErrNotFound) {
		return ""
	}
	t, ok := cat.Type(pl.ServerType)
	if !errors.Is(err, provider.ErrUnavailable) && (!ok || t.StatusIn(pl.Location) != provider.SoldOut) {
		return ""
	}
	where := "is not in it"
	if ok {
		where = availableElsewhere(t)
	}
	there := "nothing is available in " + pl.Location + " right now"
	if c := cheapestIn(cat, pl.Location, 3); c != "" {
		there = "the cheapest available in " + pl.Location + ": " + c
	}
	return fmt.Sprintf("per %s's listing, %s %s; %s; see 'devvm providers types %s --location %s'",
		providerTitle(pn), pl.ServerType, where, there, pn, pl.Location)
}

// pickOption is one select option, built as data so the picker's content
// is tested without driving huh. SoldOut marks one listed as unavailable
// now (a type listed as sold out there, a location with that type or
// nothing listed as available): listed, never the seed, and confirmed
// when chosen. Back is the type select's way back to the location select
// (backOption): never the seed, never a placement.
type pickOption struct {
	Label, Value string
	SoldOut      bool
	Back         bool
}

// pickBack is backOption's value: not a name any catalogue has (a space
// and an arrow), and askPlacementPicks never leaves it in the placement.
const pickBack = "← back"

// backOption is the type select's last option when the location was
// picked, not given by flag: a location whose types are all listed as
// sold out is not a dead end.
func backOption() pickOption {
	return pickOption{Label: "← another location", Value: pickBack, Back: true}
}

// locationOptions is the location picker: every location offering an
// offered type (offering typeName, when the type came by flag), labelled
// with its city and the number of types listed as available there and the
// cheapest (typeName's price and listing there), in the provider's order.
func locationOptions(cat *provider.Catalog, typeName string) []pickOption {
	var locs []provider.Location
	ft, flagged := cat.Type(typeName)
	for _, l := range cat.OfferingLocations() {
		if typeName == "" || (flagged && ft.StatusIn(l.Name) != provider.NotOffered) {
			locs = append(locs, l)
		}
	}
	place := func(l provider.Location) string {
		if l.City == "" {
			return ""
		}
		return l.City + ", " + l.Country
	}
	// Aligned columns, as typeOptions ("ash" beside "fsn1"), in runes as
	// fmt pads (a city may not be ASCII).
	nameW, placeW := 0, 0
	for _, l := range locs {
		nameW, placeW = max(nameW, utf8.RuneCountInString(l.Name)), max(placeW, utf8.RuneCountInString(place(l)))
	}
	plural := func(n int) string {
		if n == 1 {
			return "type"
		}
		return "types"
	}
	var out []pickOption
	for _, l := range locs {
		label := fmt.Sprintf("%-*s", nameW, l.Name)
		if placeW > 0 {
			label += fmt.Sprintf("  %-*s", placeW, place(l))
		}
		o := pickOption{Value: l.Name}
		if typeName != "" {
			p, _ := ft.PriceIn(l.Name)
			o.SoldOut = !ft.AvailableIn(l.Name)
			state := ""
			if o.SoldOut {
				state = ", sold out"
			}
			label += fmt.Sprintf("  (%s %s/mo%s)", typeName, money(cat.Currency, p.Monthly), state)
		} else {
			avail := cat.AvailableTypes(l.Name)
			o.SoldOut = len(avail) == 0
			if o.SoldOut {
				n := len(cat.Offered(l.Name))
				label += fmt.Sprintf("  (%d %s, all sold out)", n, plural(n))
			} else {
				p, _ := avail[0].PriceIn(l.Name)
				label += fmt.Sprintf("  (%d %s, from %s/mo)", len(avail), plural(len(avail)), money(cat.Currency, p.Monthly))
			}
		}
		o.Label = label
		out = append(out, o)
	}
	return out
}

// pickLabelMax is the widest a type option's label gets: a 100-column
// terminal less huh's "┃ > " and a margin. A label that wraps breaks
// huh's select (the cursor line goes missing), so the labels are compact
// and a long "available:" list is cut to fit ("+2"); a fixed width, no
// terminal-width detection.
const pickLabelMax = 94

// typeOptionsTitle is the type select's title: the labels' column names
// (they carry no units but GB, to stay within pickLabelMax).
func typeOptionsTitle(cat *provider.Catalog, loc string) string {
	return fmt.Sprintf("Server type in %s: cores, RAM, disk, %s/month (available first, cheapest first)", loc, cat.Currency)
}

// typeOptions is the type picker for loc: the offered types listed as
// available there, cheapest first, then those listed as sold out there,
// each with its specs and monthly price there in aligned columns (the
// price padded, so the sold-out labels line up too), a sold-out one
// labelled with where it is available instead.
func typeOptions(cat *provider.Catalog, loc string) []pickOption {
	types := cat.Offered(loc)
	monthly := func(t provider.ServerType) string {
		p, _ := t.PriceIn(loc)
		return fmt.Sprintf("%.2f", p.Monthly)
	}
	var nameW, cpuW, coresW, ramW, diskW, priceW int
	for _, t := range types {
		nameW, cpuW = max(nameW, len(t.Name)), max(cpuW, len(t.CPU)+1+len(t.Arch))
		coresW, ramW = max(coresW, len(strconv.Itoa(t.Cores))), max(ramW, len(gb(t.MemoryGB)))
		diskW, priceW = max(diskW, len(strconv.Itoa(t.DiskGB))), max(priceW, len(monthly(t)))
	}
	out := make([]pickOption, len(types))
	for i, t := range types {
		cores := "cores"
		if t.Cores == 1 {
			cores = "core " // the column stays aligned
		}
		label := fmt.Sprintf("%-*s  %-*s  %*d %s  %*s GB  %*d GB  %*s",
			nameW, t.Name, cpuW, t.CPU+" "+t.Arch, coresW, t.Cores, cores, ramW, gb(t.MemoryGB), diskW, t.DiskGB, priceW, monthly(t))
		soldOut := !t.AvailableIn(loc)
		if soldOut {
			label += "  sold out"
			label += availableList(t.Available, pickLabelMax-utf8.RuneCountInString(label))
		}
		out[i] = pickOption{Value: t.Name, Label: strings.TrimRight(label, " "), SoldOut: soldOut}
	}
	return out
}

// availableList is " (available: fsn1, hel1)" in at most room columns:
// as many locations as fit, then "+N" for the rest; "" for none (or no
// room for even one).
func availableList(locs []string, room int) string {
	for n := len(locs); n > 0; n-- {
		s := " (available: " + strings.Join(locs[:n], ", ")
		if n < len(locs) {
			s += fmt.Sprintf(" +%d", len(locs)-n)
		}
		if s += ")"; utf8.RuneCountInString(s) <= room {
			return s
		}
	}
	return ""
}

// seedPick is the option a select starts on: want when it is listed and
// not sold out, else the first that is not (the cheapest, for types), else
// the first; never the back option.
func seedPick(opts []pickOption, want string) string {
	for _, o := range opts {
		if o.Value == want && !o.SoldOut && !o.Back {
			return want
		}
	}
	for _, o := range opts {
		if !o.SoldOut && !o.Back {
			return o.Value
		}
	}
	for _, o := range opts {
		if !o.Back {
			return o.Value
		}
	}
	return ""
}

// soldOutQuestion is the picker's confirm for a placement listed as sold
// out, or "" when it is not.
func soldOutQuestion(cat *provider.Catalog, pl config.Placement) string {
	if t, ok := cat.Type(pl.ServerType); !ok || t.StatusIn(pl.Location) != provider.SoldOut {
		return ""
	}
	return fmt.Sprintf("%s is sold out in %s. Try anyway?", pl.ServerType, pl.Location)
}

// pickSelect and pickConfirm are the picker's two prompts, vars so a test
// can drive askPlacementPicks' loop without huh. height 0 is huh's own.
var pickSelect = func(tty *os.File, title string, opts []pickOption, height int, v *string) error {
	sel := huh.NewSelect[string]().Title(title).Options(huhOptions(opts)...).Value(v)
	if height > 0 {
		sel = sel.Height(height)
	}
	return form(tty, sel)
}

var pickConfirm = func(tty *os.File, q string) (bool, error) {
	yes := false
	err := form(tty, huh.NewConfirm().Title(q).Affirmative("Try anyway").Negative("No, pick again").Value(&yes))
	return yes, err
}

// askPlacementPicks is create's placement form with a catalogue: the grid
// first, on the terminal the prompts use, then the location, then a type
// offered there, as selects (each only when no flag gave it), then the
// image as text. A sold-out choice is allowed after one confirm (default
// No), and No goes back to the select it came from. With the location
// picked, the type select ends in "← another location", which goes back
// to the location select; every way out of the loop is a placement from
// the lists (or a confirm of one listed as sold out), an error, or the
// user's ^C. A list that comes up empty (a flag naming a type or location
// with nothing to offer, an empty catalogue) is a refusal. tried is whether the
// user confirmed a placement listed as sold out, so create does not warn
// of it again. A var so a test can answer it.
var askPlacementPicks = func(tty *os.File, pn string, s *createSpec, pl *config.Placement, cat *provider.Catalog) (tried bool, err error) {
	// confirmed asks soldOutQuestion when there is one; true goes on.
	confirmed := func() (bool, error) {
		q := soldOutQuestion(cat, *pl)
		if q == "" {
			tried = false
			return true, nil
		}
		yes, err := pickConfirm(tty, q)
		tried = yes
		return yes, err
	}
	// nothing is the answer to a list that comes up empty: checkPlacement's
	// refusal, else (it passes a deprecated type still offered, which no
	// list shows) one of its own: never a placement nobody picked, nor a
	// skipped image.
	nothing := func() error {
		if err := checkPlacement(pn, cat, *pl); err != nil {
			return err
		}
		return fmt.Errorf("%s lists nothing to pick from here; give --location and --server-type (see 'devvm providers types %s')",
			providerTitle(pn), pn)
	}
	pickLoc, pickType := s.Location == "", s.ServerType == ""
	var locOpts []pickOption
	switch {
	case pickLoc:
		locOpts = locationOptions(cat, s.ServerType)
		if len(locOpts) == 0 {
			return false, nothing()
		}
	case pickType:
		if len(typeOptions(cat, pl.Location)) == 0 {
			return false, nothing()
		}
	}
	// The grid above any select, the location by flag included: the
	// location's own table would match the select more closely, but its
	// AVAILABLE IN column makes it wider (105 columns with Hetzner's six
	// locations, against the grid's 100), so it wraps where the grid fits.
	if pickLoc || pickType {
		for _, l := range typeGrid(styleFor(tty), pn, cat) {
			fmt.Fprintln(tty, l)
		}
		fmt.Fprintln(tty)
	}
	wantLoc, wantType := pl.Location, pl.ServerType // the seeds, again after a back
	for {
		if pickLoc {
			pl.Location = seedPick(locOpts, wantLoc)
			for {
				if err := pickSelect(tty, "Location", locOpts, 0, &pl.Location); err != nil {
					return false, err
				}
				if pickType {
					break // the type select confirms
				}
				ok, err := confirmed()
				if err != nil {
					return false, err
				}
				if ok {
					break
				}
			}
			wantLoc = pl.Location
		}
		if !pickType {
			break
		}
		opts := typeOptions(cat, pl.Location)
		if len(opts) == 0 {
			return false, nothing()
		}
		if pickLoc {
			opts = append(opts, backOption())
		}
		pl.ServerType = seedPick(opts, wantType)
		back := false
		for {
			if err := pickSelect(tty, typeOptionsTitle(cat, pl.Location), opts,
				min(len(opts), 12)+2, &pl.ServerType); err != nil {
				return false, err
			}
			if pl.ServerType == pickBack {
				back = true
				break
			}
			ok, err := confirmed()
			if err != nil {
				return false, err
			}
			if ok {
				break
			}
		}
		if !back {
			break
		}
		pl.ServerType, tried = wantType, false
	}
	if s.Image == "" {
		if err := form(tty, huh.NewInput().Title("Image").Value(&pl.Image).
			Validate(func(v string) error { return config.ValidPlacement("image", strings.TrimSpace(v)) })); err != nil {
			return false, err
		}
		if pl.Image = strings.TrimSpace(pl.Image); pl.Image == "" {
			return false, fmt.Errorf("an image is required")
		}
	}
	return tried, nil
}

func huhOptions(opts []pickOption) []huh.Option[string] {
	out := make([]huh.Option[string], len(opts))
	for i, o := range opts {
		out[i] = huh.NewOption(o.Label, o.Value)
	}
	return out
}
