package hub

// Listing helpers.

import "strings"

// fieldVals: the values of a listing's lines "<name>: <value>", in order, space-joined.
func fieldVals(listing, name string) string {
	var out []string
	for _, l := range strings.Split(listing, "\n") {
		if v, ok := strings.CutPrefix(l, name+": "); ok {
			out = append(out, v)
		}
	}
	return strings.Join(out, " ")
}
