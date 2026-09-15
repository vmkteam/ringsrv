package gitlab

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenFromHeaders(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		headers []string
		want    string
		wantErr string
	}{
		"private token": {
			headers: []string{"PRIVATE-TOKEN: glpat-abc"},
			want:    "glpat-abc",
		},
		"bearer": {
			headers: []string{"Authorization: Bearer glpat-abc"},
			want:    "glpat-abc",
		},
		"header names are case-insensitive": {
			headers: []string{"private-token: glpat-abc"},
			want:    "glpat-abc",
		},
		"other headers are ignored": {
			headers: []string{"X-Scope-OrgID: prod", "PRIVATE-TOKEN: glpat-abc"},
			want:    "glpat-abc",
		},
		"basic is not a token": {
			// The catalogue-wide Authentik pass-through is Basic; it must
			// never be mistaken for a GitLab credential.
			headers: []string{"Authorization: Basic dXNlcjpwYXNz"},
			wantErr: "nothing to clone with",
		},
		"none": {
			headers: nil,
			wantErr: "nothing to clone with",
		},
		"private token without a value": {
			headers: []string{"PRIVATE-TOKEN: "},
			wantErr: "empty value",
		},
		"bearer without a value": {
			headers: []string{"Authorization: Bearer "},
			wantErr: "nothing to clone with",
		},
		"two candidates": {
			headers: []string{"PRIVATE-TOKEN: a", "Authorization: Bearer b"},
			wantErr: "want exactly one",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := TokenFromHeaders(tc.headers)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
