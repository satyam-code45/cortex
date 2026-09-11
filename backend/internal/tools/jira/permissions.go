package jira

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Write permissions on a Jira site.
//
// A Jira API token carries whatever the account behind it can do — there is no
// narrower credential to issue and no read-only variant. So enabling writes for
// a Jira connection grants nothing new; it only starts registering the write
// tools. What this file adds is honesty about the consequence: before a user is
// told writes are on, check that the account actually has the permissions, so a
// proposal is not approved by a human and then rejected by Jira.

// WritePermissions are the permission keys checked before writes are enabled.
//
// Chosen to match what the write tools actually do, no more: create issues, edit
// them, comment on them, move them through the workflow. Nothing here grants
// deletion, and no write tool performs one.
var WritePermissions = []string{
	"CREATE_ISSUES",
	"EDIT_ISSUES",
	"ADD_COMMENTS",
	"TRANSITION_ISSUES",
}

// mypermissionsResponse is the body of GET /rest/api/3/mypermissions.
type mypermissionsResponse struct {
	Permissions map[string]struct {
		HavePermission bool `json:"havePermission"`
	} `json:"permissions"`
}

// MyPermissions reports which of the requested permission keys the token's
// account holds.
//
// projectKey is optional. Jira answers global permissions site-wide, but
// issue-level permissions are per project and per permission scheme, so without
// a project the answer is "could you do this somewhere?" rather than "can you do
// this here?". Both are useful: the connection check has no project yet, and a
// proposal against a specific project can ask precisely.
func (c *Client) MyPermissions(ctx context.Context, projectKey string, keys []string) (map[string]bool, error) {
	if len(keys) == 0 {
		return map[string]bool{}, nil
	}

	query := url.Values{}
	// Required since 2020: without an explicit permissions list Jira returns
	// 400 rather than every permission, so an omitted parameter fails the check
	// for a reason that has nothing to do with the account's rights.
	query.Set("permissions", strings.Join(keys, ","))
	if key := strings.TrimSpace(projectKey); key != "" {
		query.Set("projectKey", strings.ToUpper(key))
	}

	var resp mypermissionsResponse
	if err := c.get(ctx, "/rest/api/3/mypermissions", query, &resp); err != nil {
		return nil, fmt.Errorf("read permissions: %w", err)
	}

	held := make(map[string]bool, len(keys))
	for _, key := range keys {
		held[key] = resp.Permissions[key].HavePermission
	}
	return held, nil
}

// MissingWritePermissions returns the write permissions the account lacks,
// sorted, or an empty slice when it has them all.
func (c *Client) MissingWritePermissions(ctx context.Context, projectKey string) ([]string, error) {
	held, err := c.MyPermissions(ctx, projectKey, WritePermissions)
	if err != nil {
		return nil, err
	}
	missing := make([]string, 0, len(WritePermissions))
	for key, have := range held {
		if !have {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	return missing, nil
}
