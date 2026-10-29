// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package builtins

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
)

var urlQueryParamsGeneratorType = types.MakeLabeledTuple(
	[]*types.T{types.Int, types.String, types.String, types.Bool},
	[]string{"ordinal", "name", "value", "has_equals"},
)

// urlQueryParam is a single decoded query parameter.
type urlQueryParam struct {
	// name and value are the form-decoded name and value of the parameter.
	name  string
	value string
	// hasEquals is false if the parameter did not contain an '=' character.
	hasEquals bool
}

// urlQueryParamsGenerator supports the execution of url_query_params.
type urlQueryParamsGenerator struct {
	params []urlQueryParam
	// nextOrdinal is the 1-based ordinal of the next row to return. It is 0
	// before Start has been called.
	nextOrdinal int
}

var _ eval.ValueGenerator = &urlQueryParamsGenerator{}

func makeURLQueryParamsGenerator(
	_ context.Context, _ *eval.Context, args tree.Datums,
) (eval.ValueGenerator, error) {
	rawURL := string(tree.MustBeDString(args[0]))
	params, err := parseURLQueryParams(rawURL)
	if err != nil {
		return nil, err
	}
	return &urlQueryParamsGenerator{params: params}, nil
}

// ResolvedType implements the eval.ValueGenerator interface.
func (*urlQueryParamsGenerator) ResolvedType() *types.T {
	return urlQueryParamsGeneratorType
}

// Start implements the eval.ValueGenerator interface.
func (g *urlQueryParamsGenerator) Start(_ context.Context, _ *kv.Txn) error {
	g.nextOrdinal = 0
	return nil
}

// Close implements the eval.ValueGenerator interface.
func (*urlQueryParamsGenerator) Close(_ context.Context) {}

// Next implements the eval.ValueGenerator interface.
func (g *urlQueryParamsGenerator) Next(_ context.Context) (bool, error) {
	if g.nextOrdinal >= len(g.params) {
		return false, nil
	}
	g.nextOrdinal++
	return true, nil
}

// Values implements the eval.ValueGenerator interface.
func (g *urlQueryParamsGenerator) Values() (tree.Datums, error) {
	param := g.params[g.nextOrdinal-1]
	return tree.Datums{
		tree.NewDInt(tree.DInt(g.nextOrdinal)),
		tree.NewDString(param.name),
		tree.NewDString(param.value),
		tree.MakeDBool(tree.DBool(param.hasEquals)),
	}, nil
}

// parseURLQueryParams extracts the query component from an absolute or
// relative URL and splits it into its individual parameters. It only
// inspects the input text and never accesses the network or the file
// system.
//
// The fragment (everything after the first '#') is never parsed. The
// query component starts after the first '?'. Parameters are separated by
// literal '&' characters and empty segments (caused by leading, trailing
// or consecutive '&' characters) are ignored. Each parameter is split at
// its first '=' character into a name and a value; all further '='
// characters are part of the value. A parameter without an '=' has an
// empty value and hasEquals set to false. Explicitly empty names or
// values are retained. Names and values are decoded using the
// application/x-www-form-urlencoded form encoding ('+' is a space and
// percent escapes are decoded). Duplicate parameters are preserved in
// their original order. A NULL input or a URL without a query component
// results in no parameters.
func parseURLQueryParams(rawURL string) ([]urlQueryParam, error) {
	// A '#' terminates the URL's query component: ignore the fragment.
	if hash := strings.IndexByte(rawURL, '#'); hash >= 0 {
		rawURL = rawURL[:hash]
	}
	question := strings.IndexByte(rawURL, '?')
	if question < 0 {
		// There is no query component.
		return nil, nil
	}
	query := rawURL[question+1:]
	var params []urlQueryParam
	for _, segment := range strings.Split(query, "&") {
		if segment == "" {
			// Ignore empty segments.
			continue
		}
		name := segment
		value := ""
		hasEquals := false
		// Split at the first '=' only; any subsequent '=' is part of the
		// value.
		if eq := strings.IndexByte(segment, '='); eq >= 0 {
			name = segment[:eq]
			value = segment[eq+1:]
			hasEquals = true
		}
		decodedName, err := decodeURLQueryComponent(name)
		if err != nil {
			return nil, err
		}
		decodedValue, err := decodeURLQueryComponent(value)
		if err != nil {
			return nil, err
		}
		params = append(params, urlQueryParam{
			name:      decodedName,
			value:     decodedValue,
			hasEquals: hasEquals,
		})
	}
	return params, nil
}

// decodeURLQueryComponent decodes one query parameter name or value encoded
// with the application/x-www-form-urlencoded form encoding: '+' is decoded
// as a space and '%XX' sequences as the corresponding raw byte, while all
// other bytes are preserved. It returns an error if a '%' is not followed
// by two hexadecimal digits, or if the decoded result is not valid UTF-8.
func decodeURLQueryComponent(s string) (string, error) {
	var buf []byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '+':
			if buf == nil {
				buf = make([]byte, 0, len(s))
				buf = append(buf, s[:i]...)
			}
			buf = append(buf, ' ')
		case '%':
			if i+2 >= len(s) {
				return "", pgerror.Newf(pgcode.InvalidEscapeSequence,
					"invalid URL escape in %q: "+
						"'%%' must be followed by two hexadecimal digits", s)
			}
			hi, okHi := fromURLHexDigit(s[i+1])
			lo, okLo := fromURLHexDigit(s[i+2])
			if !okHi || !okLo {
				return "", pgerror.Newf(pgcode.InvalidEscapeSequence,
					"invalid URL escape %q in %q: "+
						"'%%' must be followed by two hexadecimal digits",
					s[i:i+3], s)
			}
			if buf == nil {
				buf = make([]byte, 0, len(s))
				buf = append(buf, s[:i]...)
			}
			buf = append(buf, hi<<4|lo)
			i += 2
		default:
			if buf != nil {
				buf = append(buf, c)
			}
		}
	}
	decoded := s
	if buf != nil {
		decoded = string(buf)
	}
	if !utf8.ValidString(decoded) {
		return "", newDecodeError("UTF8")
	}
	return decoded, nil
}

// fromURLHexDigit converts a single hexadecimal ASCII digit to its value.
func fromURLHexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}
