package jira

// Test-only access to the unexported confinement, so the fuzz target can drive
// it directly instead of through an HTTP round trip per input.

// ExportScopedJQL exposes (*Client).scopedJQL to tests in this package's
// external test package.
func ExportScopedJQL(c *Client, jql string) (string, error) { return c.scopedJQL(jql) }
