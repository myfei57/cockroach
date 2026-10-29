// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package builtins

import (
	"context"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/stretchr/testify/require"
)

func TestParseURLQueryParams(t *testing.T) {
	type expectedRow struct {
		ordinal   int
		name      string
		value     string
		hasEquals bool
	}
	testCases := []struct {
		name     string
		url      string
		expected []expectedRow
	}{
		{
			name: "spec example",
			url:  "/x?tag=a&tag=b&flag&empty=",
			expected: []expectedRow{
				{1, "tag", "a", true},
				{2, "tag", "b", true},
				{3, "flag", "", false},
				{4, "empty", "", true},
			},
		},
		{
			name: "absolute URL with fragment",
			url:  "https://user:pw@example.com:8443/p/to?a=1&b=2#section?ignored=1",
			expected: []expectedRow{
				{1, "a", "1", true},
				{2, "b", "2", true},
			},
		},
		{
			name:     "relative URL without query",
			url:      "/a/b/c",
			expected: nil,
		},
		{
			name:     "plain string",
			url:      "hello",
			expected: nil,
		},
		{
			name:     "empty string",
			url:      "",
			expected: nil,
		},
		{
			name:     "lone question mark",
			url:      "?",
			expected: nil,
		},
		{
			name:     "fragment present before query",
			url:      "/x#frag?a=1",
			expected: nil,
		},
		{
			name: "fragment terminates query",
			url:  "/x?a=1#b=2&c=3",
			expected: []expectedRow{
				{1, "a", "1", true},
			},
		},
		{
			name: "empty segments are ignored",
			url:  "?&&a=1&&b=2&",
			expected: []expectedRow{
				{1, "a", "1", true},
				{2, "b", "2", true},
			},
		},
		{
			name: "only the first equals separates",
			url:  "?a=b=c",
			expected: []expectedRow{
				{1, "a", "b=c", true},
			},
		},
		{
			name: "question mark in value is literal",
			url:  "?q=a?b",
			expected: []expectedRow{
				{1, "q", "a?b", true},
			},
		},
		{
			name: "explicit empty name",
			url:  "?=x",
			expected: []expectedRow{
				{1, "", "x", true},
			},
		},
		{
			name: "explicit empty value",
			url:  "?a=",
			expected: []expectedRow{
				{1, "a", "", true},
			},
		},
		{
			name: "both name and value empty",
			url:  "?=",
			expected: []expectedRow{
				{1, "", "", true},
			},
		},
		{
			name: "plus decodes as space in name and value",
			url:  "?a+b=c+d",
			expected: []expectedRow{
				{1, "a b", "c d", true},
			},
		},
		{
			name: "percent escapes including encoded separators",
			url:  "?na%20me=va%3Dlue%26%2f",
			expected: []expectedRow{
				{1, "na me", "va=lue&/", true},
			},
		},
		{
			name: "uppercase hex and literal plus sign",
			url:  "?%2B=%E4%BD%A0%E5%A5%BD",
			expected: []expectedRow{
				{1, "+", "你好", true},
			},
		},
		{
			name: "semicolon is an ordinary character",
			url:  "?a=b;c=d",
			expected: []expectedRow{
				{1, "a", "b;c=d", true},
			},
		},
		{
			name: "encoded ampersand and equals are not separators",
			url:  "?a=b%26c%3Dd",
			expected: []expectedRow{
				{1, "a", "b&c=d", true},
			},
		},
		{
			name: "raw multibyte utf8 is preserved",
			url:  "?q=你好",
			expected: []expectedRow{
				{1, "q", "你好", true},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			params, err := parseURLQueryParams(tc.url)
			require.NoError(t, err)
			require.Len(t, params, len(tc.expected))
			for i, p := range params {
				exp := tc.expected[i]
				require.Equal(t, exp.ordinal, i+1, "ordinal for parameter %d", i)
				require.Equal(t, exp.name, p.name, "name for parameter %d", i)
				require.Equal(t, exp.value, p.value, "value for parameter %d", i)
				require.Equal(t, exp.hasEquals, p.hasEquals, "hasEquals for parameter %d", i)
			}
		})
	}
}

func TestParseURLQueryParamsErrors(t *testing.T) {
	testCases := []struct {
		name     string
		url      string
		expected pgcode.Code
	}{
		{"invalid hex digits", "/x?a=%zz", pgcode.InvalidEscapeSequence},
		{"truncated escape with one digit", "/x?a=%4", pgcode.InvalidEscapeSequence},
		{"lone percent in name", "/x?a%=1", pgcode.InvalidEscapeSequence},
		{"lone percent", "/x?%", pgcode.InvalidEscapeSequence},
		{"invalid utf8 single byte", "/x?a=%FF", pgcode.CharacterNotInRepertoire},
		{"invalid utf8 truncated sequence", "/x?a=%E4%BD", pgcode.CharacterNotInRepertoire},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseURLQueryParams(tc.url)
			require.Error(t, err)
			require.Equal(t, tc.expected, pgerror.GetPGCode(err))
		})
	}
}

func TestURLQueryParamsGenerator(t *testing.T) {
	gen, err := makeURLQueryParamsGenerator(
		context.Background(), nil, /* evalCtx */
		tree.Datums{tree.NewDString("/x?tag=a&flag")},
	)
	require.NoError(t, err)
	require.Equal(t, urlQueryParamsGeneratorType, gen.ResolvedType())
	require.NoError(t, gen.Start(context.Background(), nil /* txn */))
	defer gen.Close(context.Background())

	expected := []struct {
		ordinal   int
		name      string
		value     string
		hasEquals bool
	}{
		{1, "tag", "a", true},
		{2, "flag", "", false},
	}
	for _, exp := range expected {
		hasNext, err := gen.Next(context.Background())
		require.NoError(t, err)
		require.True(t, hasNext)
		values, err := gen.Values()
		require.NoError(t, err)
		require.Len(t, values, 4)
		require.Equal(t, tree.NewDInt(tree.DInt(exp.ordinal)), values[0])
		require.Equal(t, tree.NewDString(exp.name), values[1])
		require.Equal(t, tree.NewDString(exp.value), values[2])
		require.Equal(t, tree.MakeDBool(tree.DBool(exp.hasEquals)), values[3])
	}
	hasNext, err := gen.Next(context.Background())
	require.NoError(t, err)
	require.False(t, hasNext)
}
