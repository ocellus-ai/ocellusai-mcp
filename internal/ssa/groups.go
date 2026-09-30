package ssa

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ParseGroups parses "0;1,2;3-5" into groups of component indices: groups
// are separated by ';', a group lists numbers and ranges separated by ','.
// Spaces are ignored; a number listed twice in one group is an error. An
// empty spec gives nil.
func ParseGroups(spec string) ([][]int, error) {
	var groups [][]int
	for part := range strings.SplitSeq(spec, ";") {
		part = strings.ReplaceAll(part, " ", "")
		if part == "" {
			continue
		}
		var g []int
		for item := range strings.SplitSeq(part, ",") {
			a, b, isRange := strings.Cut(item, "-")
			lo, err := strconv.Atoi(a)
			hi := lo
			if err == nil && isRange {
				hi, err = strconv.Atoi(b)
			}
			if err != nil || lo < 0 || hi < lo {
				return nil, fmt.Errorf("bad item %q (want a component number or a range such as 3-5)", item)
			}
			for i := lo; i <= hi; i++ {
				if slices.Contains(g, i) {
					return nil, fmt.Errorf("component %d listed twice in %q", i, part)
				}
				g = append(g, i)
			}
		}
		groups = append(groups, g)
	}
	return groups, nil
}

// FormatGroup writes component numbers compactly, as ParseGroups takes them:
// 0,3-5.
func FormatGroup(g []int) string {
	c := slices.Clone(g)
	slices.Sort(c)
	c = slices.Compact(c)
	var parts []string
	for i := 0; i < len(c); {
		j := i
		for j+1 < len(c) && c[j+1] == c[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("%d-%d", c[i], c[j]))
		} else {
			parts = append(parts, strconv.Itoa(c[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// FormatGroups writes several groups separated by ';'.
func FormatGroups(gs [][]int) string {
	parts := make([]string, len(gs))
	for i, g := range gs {
		parts[i] = FormatGroup(g)
	}
	return strings.Join(parts, ";")
}

// Union returns the sorted union of the groups: they may overlap.
func Union(groups [][]int) []int {
	var all []int
	for _, g := range groups {
		all = append(all, g...)
	}
	slices.Sort(all)
	return slices.Compact(all)
}
