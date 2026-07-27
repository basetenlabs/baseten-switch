package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type simpleFlag struct {
	name     string
	env      string
	def      string
	isBool   bool
	isInt    bool
	val      string
	wasSet   bool
	helpText string
}

type simpleFlagSet struct {
	flags []*simpleFlag
}

func newSimpleFlagSet() *simpleFlagSet { return &simpleFlagSet{} }

func (f *simpleFlagSet) string(name, env, def, help string) {
	f.flags = append(f.flags, &simpleFlag{name: name, env: env, def: def, helpText: help})
}

func (f *simpleFlagSet) bool(name, env string, def bool, help string) {
	d := "false"
	if def {
		d = "true"
	}
	f.flags = append(f.flags, &simpleFlag{name: name, env: env, def: d, isBool: true, helpText: help})
}

func (f *simpleFlagSet) int(name string, def int, help string) {
	f.flags = append(f.flags, &simpleFlag{name: name, def: strconv.Itoa(def), isInt: true, helpText: help})
}

func (f *simpleFlagSet) lookupString(name string) string {
	for _, fl := range f.flags {
		if fl.name == name {
			return fl.val
		}
	}
	return ""
}

func (f *simpleFlagSet) lookupBool(name string) bool {
	for _, fl := range f.flags {
		if fl.name == name {
			return fl.val == "true"
		}
	}
	return false
}

func (f *simpleFlagSet) lookupInt(name string) int {
	for _, fl := range f.flags {
		if fl.name == name {
			n, _ := strconv.Atoi(fl.val)
			return n
		}
	}
	return 0
}

func (f *simpleFlagSet) parse(args []string) error {
	for _, fl := range f.flags {
		fl.val = fl.def
		if fl.env != "" {
			if v := os.Getenv(fl.env); v != "" {
				fl.val = v
				fl.wasSet = true
			}
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "" || a[0] != '-' {
			return fmt.Errorf("unexpected positional arg: %s", a)
		}
		key := strings.TrimPrefix(a, "--")
		var val string
		if idx := strings.Index(key, "="); idx >= 0 {
			val = key[idx+1:]
			key = key[:idx]
		}
		fl := f.find(key)
		if fl == nil {
			return fmt.Errorf("unknown flag: %s", a)
		}
		if fl.isBool && val == "" {
			fl.val = "true"
			fl.wasSet = true
			continue
		}
		if val == "" {
			if i+1 >= len(args) {
				return fmt.Errorf("flag --%s requires a value", key)
			}
			i++
			val = args[i]
		}
		fl.val = val
		fl.wasSet = true
	}
	return nil
}

func (f *simpleFlagSet) find(name string) *simpleFlag {
	for _, fl := range f.flags {
		if fl.name == name {
			return fl
		}
	}
	return nil
}
