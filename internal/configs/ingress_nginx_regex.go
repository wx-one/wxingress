package configs

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/dlclark/regexp2/v2"
)

// pcreExactCount is a quantifier {n} at the start of the text.
var pcreExactCount = regexp.MustCompile(`^\{[0-9]+\}`)

// CheckPCRERegex returns an error when nginx's PCRE would not compile the
// regular expression. The controller compiles paths and patterns with
// regexp2, whose syntax is that of .NET: it takes constructs PCRE refuses,
// which would make nginx -t fail and with it the reload of every resource.
// Besides what regexp2 refuses, it refuses
//   - balancing groups, (?<name-other>...), (?<-other>...) and the same with quotes
//   - group names that start with a digit, (?<1>...)
//   - conditions that are no group, (?(expression)yes|no), which .NET takes as a lookahead
//   - a quantifier in a lookbehind other than {n}, which .NET takes and PCRE does not (it must have a fixed length)
//   - the Unicode blocks of .NET, \p{IsGreek}
func CheckPCRERegex(pattern string) error {
	if _, err := regexp2.Compile(pattern); err != nil {
		return fmt.Errorf("it is no valid regular expression: %w", err)
	}
	names := pcreGroupNames(pattern)
	// lookbehinds holds, for each group that is open, whether it is (in) a lookbehind
	var lookbehinds []bool
	inLookbehind := func() bool { return len(lookbehinds) > 0 && lookbehinds[len(lookbehinds)-1] }
	inClass := false
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c == '\\' {
			if i+1 < len(pattern) && (pattern[i+1] == 'p' || pattern[i+1] == 'P') && strings.HasPrefix(pattern[i+2:], "{Is") {
				return errors.New(`the Unicode blocks of .NET (\p{Is...}) are not supported by PCRE`)
			}
			i++
			continue
		}
		if inClass {
			if c == ']' {
				inClass = false
			}
			continue
		}
		switch c {
		case '[':
			inClass = true
			// a ] right after [ or [^ is a character of the class
			if strings.HasPrefix(pattern[i+1:], "^]") {
				i += 2
			} else if strings.HasPrefix(pattern[i+1:], "]") {
				i++
			}
		case ')':
			if len(lookbehinds) > 0 {
				lookbehinds = lookbehinds[:len(lookbehinds)-1]
			}
		case '*', '+', '?', '{':
			if inLookbehind() && !(c == '?' && i > 0 && pattern[i-1] == '(') && !(c == '{' && pcreExactCount.MatchString(pattern[i:])) {
				return errors.New("a lookbehind must have a fixed length in PCRE: it must not have a quantifier")
			}
		case '(':
			rest := pattern[i+1:]
			lookbehind := inLookbehind() || strings.HasPrefix(rest, "?<=") || strings.HasPrefix(rest, "?<!")
			lookbehinds = append(lookbehinds, lookbehind)
			if err := checkPCREGroup(rest, names); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkPCREGroup checks the group whose text after its "(" is rest.
func checkPCREGroup(rest string, names map[string]bool) error {
	var name string
	switch {
	case strings.HasPrefix(rest, "?<=") || strings.HasPrefix(rest, "?<!"):
		return nil
	case strings.HasPrefix(rest, "?<"):
		name, _, _ = strings.Cut(rest[2:], ">")
	case strings.HasPrefix(rest, "?'"):
		name, _, _ = strings.Cut(rest[2:], "'")
	case strings.HasPrefix(rest, "?("):
		condition, _, _ := strings.Cut(rest[2:], ")")
		if !isPCRECondition(condition, names) {
			return fmt.Errorf("the condition (?(%s) is no group: PCRE takes only a group number, a name, R or a lookaround", condition)
		}
		return nil
	default:
		return nil
	}
	if strings.Contains(name, "-") {
		return fmt.Errorf("the balancing group (?<%s>) of .NET is not supported by PCRE", name)
	}
	if name != "" && name[0] >= '0' && name[0] <= '9' {
		return fmt.Errorf("the group name %q starts with a digit, which PCRE does not take", name)
	}
	return nil
}

// isPCRECondition tells whether PCRE takes the condition of (?(condition)yes|no).
func isPCRECondition(condition string, names map[string]bool) bool {
	switch {
	case condition == "":
		return false
	case strings.Trim(condition, "0123456789") == "", strings.Trim(strings.TrimLeft(condition, "+-"), "0123456789") == "":
		return true
	case strings.HasPrefix(condition, "?=") || strings.HasPrefix(condition, "?!") ||
		strings.HasPrefix(condition, "?<=") || strings.HasPrefix(condition, "?<!"):
		return true
	case condition == "R" || condition == "DEFINE" || strings.HasPrefix(condition, "R&") ||
		strings.HasPrefix(condition, "R") && strings.Trim(condition[1:], "0123456789") == "":
		return true
	case strings.HasPrefix(condition, "<") && strings.HasSuffix(condition, ">"):
		return names[condition[1:len(condition)-1]]
	case strings.HasPrefix(condition, "'") && strings.HasSuffix(condition, "'") && len(condition) > 1:
		return names[condition[1:len(condition)-1]]
	}
	return names[condition]
}

// pcreGroupNames are the names of the named groups of the pattern.
func pcreGroupNames(pattern string) map[string]bool {
	names := map[string]bool{}
	for i := 0; i+2 < len(pattern); i++ {
		if pattern[i] == '\\' {
			i++
			continue
		}
		if pattern[i] != '(' || pattern[i+1] != '?' {
			continue
		}
		rest := pattern[i+2:]
		var end byte
		switch {
		case strings.HasPrefix(rest, "P<"):
			rest, end = rest[2:], '>'
		case strings.HasPrefix(rest, "<") && !strings.HasPrefix(rest, "<=") && !strings.HasPrefix(rest, "<!"):
			rest, end = rest[1:], '>'
		case strings.HasPrefix(rest, "'"):
			rest, end = rest[1:], '\''
		default:
			continue
		}
		if j := strings.IndexByte(rest, end); j > 0 {
			names[rest[:j]] = true
		}
	}
	return names
}
