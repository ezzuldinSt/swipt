package device

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
)

var (
	stringFields   = []string{"role", "class", "label", "value", "identifier", "placeholder", "hint", "url", "app"}
	flagFields     = []string{"selected", "focused", "disabled", "secure", "partial", "offScreen", "inert", "remote"}
	relationFields = []string{"within", "parent", "has", "not"}
	selectorFields = slices.Concat(stringFields, []string{"traits"}, flagFields, []string{"expanded"}, relationFields, []string{"index"})
)

var ErrScripted = E.New("the selector holds a function or a regular expression only JavaScript can run")

type Callbacks interface {
	Predicate(id int, element *ViewElement) (bool, error)
	Match(id int, text string) (bool, error)
}

type Selector struct {
	predicate int
	fields    []selectorField
	index     *int
	scripted  bool
}

type selectorField struct {
	name     string
	text     *textMatch
	traits   []string
	flag     bool
	relation *Selector
	element  string
}

type textMatch struct {
	text    string
	pattern *regexp.Regexp
	source  string
	flags   string
	script  int
	regular bool
}

func ParseSelector(data []byte) (*Selector, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	selector, err := parseSelector(decoder)
	if err != nil {
		return nil, err
	}
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, E.Extend(ErrInvalid, "the selector is not one JSON value")
	}
	return selector, nil
}

func selectorError(message ...any) error {
	return E.Extend(ErrInvalid, message...)
}

func parseSelector(decoder *json.Decoder) (*Selector, error) {
	fields, err := readObject(decoder, "expected a selector: an object of element fields, or a function of an element")
	if err != nil {
		return nil, err
	}
	if id, isPredicate := fields.number("$predicate"); isPredicate {
		return &Selector{predicate: id, scripted: true}, nil
	}
	selector := &Selector{predicate: -1}
	for _, entry := range fields {
		if entry.key == "index" {
			var at json.Number
			if json.Unmarshal(entry.value, &at) != nil {
				return nil, selectorError("index must be a non-negative integer")
			}
			index, err := strconv.Atoi(at.String())
			if err != nil || index < 0 {
				return nil, selectorError("index must be a non-negative integer")
			}
			selector.index = &index
			continue
		}
		field, err := parseField(entry.key, entry.value)
		if err != nil {
			return nil, err
		}
		selector.scripted = selector.scripted || field.scripted()
		selector.fields = append(selector.fields, field)
	}
	if len(selector.fields) == 0 {
		return nil, selectorError("the selector is empty")
	}
	return selector, nil
}

func (f selectorField) scripted() bool {
	return f.text != nil && f.text.pattern == nil && f.text.regular || f.relation != nil && f.relation.scripted
}

func parseField(name string, value json.RawMessage) (selectorField, error) {
	field := selectorField{name: name}
	switch {
	case name == "traits":
		if json.Unmarshal(value, &field.traits) != nil || field.traits == nil {
			return field, selectorError("traits must be an array of strings")
		}
	case slices.Contains(relationFields, name):
		var element struct {
			Ref *string `json:"$element"`
		}
		if json.Unmarshal(value, &element) == nil && element.Ref != nil {
			field.element = *element.Ref
			return field, nil
		}
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.UseNumber()
		relation, err := parseSelector(decoder)
		if err != nil {
			return field, err
		}
		if name == "not" && relation.index != nil {
			return field, selectorError("the selector under not cannot have an index")
		}
		field.relation = relation
	case slices.Contains(flagFields, name), name == "expanded":
		if json.Unmarshal(value, &field.flag) != nil {
			return field, selectorError(name, " must be a boolean")
		}
	case slices.Contains(stringFields, name):
		match, err := parseText(name, value)
		if err != nil {
			return field, err
		}
		field.text = match
	default:
		return field, selectorError("unknown element field ", strconv.Quote(name), "; the fields are ", strings.Join(selectorFields, ", "))
	}
	return field, nil
}

func parseText(name string, value json.RawMessage) (*textMatch, error) {
	var text string
	if json.Unmarshal(value, &text) == nil {
		return &textMatch{text: plainText(text), script: -1}, nil
	}
	var pattern struct {
		Source *string `json:"$regex"`
		Flags  string  `json:"$flags"`
		ID     int     `json:"$id"`
	}
	if json.Unmarshal(value, &pattern) != nil || pattern.Source == nil {
		return nil, selectorError(name, " must be a string or a RegExp")
	}
	match := &textMatch{regular: true, source: *pattern.Source, flags: pattern.Flags, script: pattern.ID}
	match.pattern, _ = compileScript(*pattern.Source, pattern.Flags)
	return match, nil
}

func compileScript(source, flags string) (*regexp.Regexp, error) {
	var out strings.Builder
	var modes strings.Builder
	for _, flag := range flags {
		switch flag {
		case 'i', 'm', 's':
			modes.WriteRune(flag)
		case 'g', 'y', 'd', 'u', 'v':
		default:
			return nil, E.New("unknown flag ", string(flag))
		}
	}
	if modes.Len() > 0 {
		out.WriteString("(?" + modes.String() + ")")
	}
	runes := []rune(source)
	for index := 0; index < len(runes); index++ {
		r := runes[index]
		if r != '\\' || index+1 >= len(runes) {
			if r == '[' && index+2 < len(runes) && runes[index+1] == '^' && runes[index+2] == ']' {
				out.WriteString(`[\x{0}-\x{10FFFF}]`)
				index += 2
				continue
			}
			out.WriteRune(r)
			continue
		}
		next := runes[index+1]
		switch {
		case next == 'u' && index+2 < len(runes) && runes[index+2] == '{':
			end := slices.Index(runes[index+3:], '}')
			if end < 0 {
				return nil, E.New("unterminated code point escape")
			}
			out.WriteString(`\x{` + string(runes[index+3:index+3+end]) + `}`)
			index += 3 + end
		case next == 'u' && index+5 < len(runes) && common.All(runes[index+2:index+6], isHex):
			out.WriteString(`\x{` + string(runes[index+2:index+6]) + `}`)
			index += 5
		case next == '/':
			out.WriteRune('/')
			index++
		default:
			out.WriteRune(r)
			out.WriteRune(next)
			index++
		}
	}
	return regexp.Compile(out.String())
}

func isHex(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
}

type objectEntry struct {
	key   string
	value json.RawMessage
}

type object []objectEntry

func (o object) number(key string) (int, bool) {
	for _, entry := range o {
		if entry.key == key {
			var number int
			if json.Unmarshal(entry.value, &number) == nil {
				return number, true
			}
		}
	}
	return 0, false
}

func readObject(decoder *json.Decoder, message string) (object, error) {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, selectorError(message)
	}
	var out object
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, selectorError(message)
		}
		key, _ := token.(string)
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return nil, selectorError(message)
		}
		out = append(out, objectEntry{key: key, value: value})
	}
	if _, err = decoder.Token(); err != nil {
		return nil, selectorError(message)
	}
	return out, nil
}

func (s *Selector) Scripted() bool {
	return s.scripted
}

func (s *Selector) Fields() []string {
	var fields []string
	for _, field := range s.fields {
		if field.relation != nil {
			fields = append(fields, field.relation.Fields()...)
		} else {
			fields = append(fields, field.name)
		}
	}
	return fields
}

func (s *Selector) App() string {
	for _, field := range s.fields {
		if field.name == "app" && !field.text.regular {
			return field.text.text
		}
	}
	return ""
}

func (s *Selector) String() string {
	if s.predicate >= 0 {
		return "the selector function"
	}
	parts := common.Map(s.fields, selectorField.String)
	if s.index != nil {
		parts = append(parts, "index: "+strconv.Itoa(*s.index))
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

func (f selectorField) String() string {
	switch {
	case f.text != nil && f.text.regular:
		return f.name + ": /" + f.text.source + "/" + f.text.flags
	case f.text != nil:
		return f.name + ": " + quote(f.text.text)
	case f.traits != nil:
		data, _ := marshalText(f.traits)
		return f.name + ": " + data
	case f.relation != nil:
		return f.name + ": " + f.relation.String()
	case f.element != "":
		return f.name + ": an element"
	}
	return f.name + ": " + strconv.FormatBool(f.flag)
}

func plainText(text string) string {
	return strings.TrimFunc(PlainText(text), func(r rune) bool { return unicode.IsSpace(r) || r == '\uFEFF' })
}

func (v *View) FindAll(scope string, selector *Selector, callbacks Callbacks) ([]*ViewElement, error) {
	domain, universe, err := v.matching().domain(scope)
	if err != nil {
		return nil, err
	}
	return selectElements(domain, selector, universe, callbacks)
}

func (v *View) Find(scope string, selector *Selector, callbacks Callbacks) (*ViewElement, error) {
	found, err := v.FindAll(scope, selector, callbacks)
	if err != nil || len(found) == 0 {
		return nil, err
	}
	if len(found) > 1 {
		return nil, E.New(selector.String(), " is not unique: ", len(found), " elements match: ", candidates(found))
	}
	return found[0], nil
}

// Best finds the match an action most likely means, ranked by rankOf, and
// those it cannot tell from it, all in the order of the tree; it answers no
// elements when none matches.
func (v *View) Best(scope string, selector *Selector, callbacks Callbacks) ([]*ViewElement, error) {
	found, err := v.FindAll(scope, selector, callbacks)
	if err != nil || len(found) < 2 {
		return found, err
	}
	return best(found), nil
}

// Unique finds the one match an action means, failing when the ranking cannot
// tell several apart rather than acting on one of them blindly.
func (v *View) Unique(scope string, selector *Selector, callbacks Callbacks) (*ViewElement, error) {
	found, err := v.Best(scope, selector, callbacks)
	switch {
	case err != nil || len(found) == 0:
		return nil, err
	case len(found) > 1:
		return nil, E.New(selector.String(), " matches ", len(found), " elements that rank alike: ", candidates(found), "; add a field, within or index to pick one")
	}
	return found[0], nil
}

type rank struct {
	flags int
	area  float64
}

// A match is likelier the one meant when it shows on the screen, then when
// nothing covers it and no modal view keeps it inert, then when it is enabled,
// then when it is a control or holds actions rather than text or a container;
// among those alike, one at least twice the size of another wins.
const rankSize = 2

func rankOf(element *ViewElement) rank {
	node := element.node
	shown := node != nil && node.Shown()
	out := rank{}
	for _, flag := range []bool{
		shown,
		shown && !node.Covered && !element.Inert,
		!element.Disabled,
		isControl(element.Role) || element.Role == "cell" || len(element.Actions) > 0 ||
			slices.Contains(element.Traits, "button") || slices.Contains(element.Traits, "link"),
	} {
		out.flags <<= 1
		if flag {
			out.flags |= 1
		}
	}
	if shown {
		out.area = node.Visible.area()
	}
	return out
}

func best(found []*ViewElement) []*ViewElement {
	ranks := make([]rank, len(found))
	top := rank{flags: -1}
	for index, element := range found {
		ranks[index] = rankOf(element)
		if it := ranks[index]; it.flags > top.flags || it.flags == top.flags && it.area > top.area {
			top = it
		}
	}
	var out []*ViewElement
	for index, element := range found {
		if it := ranks[index]; it.flags == top.flags && (it.area == top.area || it.area*rankSize > top.area) {
			out = append(out, element)
		}
	}
	return out
}

const listedCandidates = 6

func candidates(found []*ViewElement) string {
	parts := make([]string, 0, min(len(found), listedCandidates))
	for _, element := range found[:min(len(found), listedCandidates)] {
		parts = append(parts, element.String())
	}
	out := strings.Join(parts, "; ")
	if len(found) > listedCandidates {
		out += "; and " + strconv.Itoa(len(found)-listedCandidates) + " more"
	}
	return out
}

// String describes the element on one line, as the line of the JavaScript
// element's toString.
func (e *ViewElement) String() string {
	parts := []string{e.Role}
	if e.Label != "" {
		parts = append(parts, quote(clipText(e.Label)))
	}
	if e.ShownValue != nil && *e.ShownValue != "" {
		parts = append(parts, "value="+quote(clipText(*e.ShownValue)))
	}
	if e.Identifier != "" {
		parts = append(parts, "id="+quote(e.Identifier))
	}
	if e.Frame != nil {
		parts = append(parts, "["+strings.Join(common.Map(e.Frame[:], func(value float64) string { return strconv.Itoa(int(math.Round(value))) }), ",")+"]")
	}
	for _, flag := range lineFlags {
		if flag.set(e) {
			parts = append(parts, flag.name)
		}
	}
	if e.node != nil && e.node.Covered {
		parts = append(parts, "covered")
	}
	return strings.Join(parts, " ")
}

func (v *View) domain(scope string) (domain, universe []*ViewElement, err error) {
	if scope == "" {
		all := v.all()
		return all, all, nil
	}
	element := v.refs[scope]
	if element == nil {
		return nil, nil, E.Extend(ErrNotFound, "the element is not in this read of the screen")
	}
	root := element
	for root.parent != nil {
		root = root.parent
	}
	return flatten(element)[1:], flatten(root), nil
}

func selectElements(elements []*ViewElement, selector *Selector, universe []*ViewElement, callbacks Callbacks) ([]*ViewElement, error) {
	match, err := matcher(selector, universe, callbacks)
	if err != nil {
		return nil, err
	}
	var found []*ViewElement
	matches := make(map[*ViewElement]bool)
	for _, element := range elements {
		matched, err := match(element)
		if err != nil {
			return nil, err
		}
		if matched {
			found = append(found, element)
			matches[element] = true
		}
	}
	found = slices.DeleteFunc(found, func(element *ViewElement) bool { return repeatsAncestor(element, matches) })
	if selector.index == nil {
		return found, nil
	}
	if *selector.index < len(found) {
		return found[*selector.index : *selector.index+1], nil
	}
	return nil, nil
}

func matcher(selector *Selector, universe []*ViewElement, callbacks Callbacks) (func(*ViewElement) (bool, error), error) {
	if selector.predicate >= 0 {
		if callbacks == nil {
			return nil, ErrScripted
		}
		return func(element *ViewElement) (bool, error) {
			return callbacks.Predicate(selector.predicate, element)
		}, nil
	}
	checks := make([]func(*ViewElement) (bool, error), 0, len(selector.fields))
	for _, field := range selector.fields {
		check, err := field.check(universe, callbacks)
		if err != nil {
			return nil, err
		}
		checks = append(checks, check)
	}
	return func(element *ViewElement) (bool, error) {
		for _, check := range checks {
			matched, err := check(element)
			if err != nil || !matched {
				return false, err
			}
		}
		return true, nil
	}, nil
}

func (f selectorField) check(universe []*ViewElement, callbacks Callbacks) (func(*ViewElement) (bool, error), error) {
	switch {
	case f.traits != nil:
		return func(element *ViewElement) (bool, error) {
			return common.All(f.traits, func(trait string) bool { return slices.Contains(element.Traits, trait) }), nil
		}, nil
	case f.name == "not" && f.relation != nil:
		match, err := matcher(f.relation, universe, callbacks)
		if err != nil {
			return nil, err
		}
		return func(element *ViewElement) (bool, error) {
			matched, err := match(element)
			return !matched, err
		}, nil
	case f.relation != nil || f.element != "":
		scope := make(map[*ViewElement]bool)
		if f.element != "" {
			for _, element := range universe {
				if element.Ref == f.element {
					scope[element] = true
				}
			}
		} else {
			found, err := selectElements(universe, f.relation, universe, callbacks)
			if err != nil {
				return nil, err
			}
			for _, element := range found {
				scope[element] = true
			}
		}
		return func(element *ViewElement) (bool, error) { return related(f.name, element, scope), nil }, nil
	case f.name == "expanded":
		return func(element *ViewElement) (bool, error) {
			return element.Expanded != nil && *element.Expanded == f.flag, nil
		}, nil
	case f.text == nil:
		index := slices.Index(flagFields, f.name)
		return func(element *ViewElement) (bool, error) { return lineFlags[index].set(element) == f.flag, nil }, nil
	}
	return func(element *ViewElement) (bool, error) {
		text, present := element.text(f.name)
		if !present {
			return false, nil
		}
		return f.text.match(plainText(text), callbacks)
	}, nil
}

func repeatsAncestor(element *ViewElement, matches map[*ViewElement]bool) bool {
	if element.Label == "" {
		return false
	}
	for ancestor := element.parent; ancestor != nil; ancestor = ancestor.parent {
		if matches[ancestor] && sameText(ancestor.Label, element.Label) {
			return true
		}
	}
	return false
}

func related(relation string, element *ViewElement, scope map[*ViewElement]bool) bool {
	switch relation {
	case "parent":
		return element.parent != nil && scope[element.parent]
	case "has":
		return slices.ContainsFunc(flatten(element)[1:], func(it *ViewElement) bool { return scope[it] })
	case "not":
		return !scope[element]
	}
	for ancestor := element.parent; ancestor != nil; ancestor = ancestor.parent {
		if scope[ancestor] {
			return true
		}
	}
	return false
}

func (e *ViewElement) text(field string) (string, bool) {
	switch field {
	case "role":
		return e.Role, true
	case "class":
		return e.Class, e.Class != ""
	case "label":
		return e.Label, e.Label != ""
	case "value":
		if e.ShownValue == nil {
			return "", false
		}
		return *e.ShownValue, true
	case "identifier":
		return e.Identifier, e.Identifier != ""
	case "placeholder":
		return e.Placeholder, e.Placeholder != ""
	case "hint":
		return e.Hint, e.Hint != ""
	case "url":
		return e.URL, e.URL != ""
	case "app":
		return e.app, true
	}
	return "", false
}

func (m *textMatch) match(text string, callbacks Callbacks) (bool, error) {
	switch {
	case !m.regular:
		return text == m.text, nil
	case m.pattern != nil:
		return m.pattern.MatchString(text), nil
	case callbacks == nil:
		return false, ErrScripted
	}
	return callbacks.Match(m.script, text)
}
