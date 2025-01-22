// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cat

import (
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
)

// PolicyCommandScope specifies the scope of SQL commands to which a policy applies.
// It determines whether a policy is enforced for specific operations or if an operation
// is exempt from row-level security. The operations checked must align with the policy
// commands defined in the CREATE POLICY SQL syntax.
type PolicyCommandScope int

const (
	// PolicyScopeSelect indicates that the policy applies to SELECT operations.
	PolicyScopeSelect PolicyCommandScope = iota
	// PolicyScopeInsert indicates that the policy applies to INSERT operations.
	PolicyScopeInsert
	// PolicyScopeUpdate indicates that the policy applies to UPDATE operations.
	PolicyScopeUpdate
	// PolicyScopeDelete indicates that the policy applies to DELETE operations.
	PolicyScopeDelete
	// PolicyScopeExempt indicates that the operation is exempt from row-level security policies.
	PolicyScopeExempt
)

// Policy defines an interface for a row-level security (RLS) policy on a table.
// Policies use expressions to filter rows during read operations and/or restrict
// new rows during write operations.
type Policy interface {
	// Name returns the name of the policy. The name is unique within a table
	// and cannot be qualified.
	Name() tree.Name

	// GetUsingExpr returns the optional filter expression evaluated on rows during
	// read operations. If the policy does not define a USING expression, this returns
	// an empty string.
	GetUsingExpr() string

	// GetWithCheckExpr returns the optional validation expression applied to new rows
	// during write operations. If the policy does not define a WITH CHECK expression,
	// this returns an empty string.
	GetWithCheckExpr() string

	// GetPolicyCommand returns the command that the policy was defined for.
	GetPolicyCommand() catpb.PolicyCommand

	// AppliesToRole checks whether the policy applies to the give role.
	AppliesToRole(user username.SQLUsername) bool
}
