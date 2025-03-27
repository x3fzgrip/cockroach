// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package jsonpath

import "fmt"

type OperationType int

const (
	OpCompEqual OperationType = iota
	OpCompNotEqual
	OpCompLess
	OpCompLessEqual
	OpCompGreater
	OpCompGreaterEqual
	OpLogicalAnd
	OpLogicalOr
	OpLogicalNot
	OpAdd
	OpSub
	OpMult
	OpDiv
	OpMod
	OpLikeRegex
)

var OperationTypeStrings = map[OperationType]string{
	OpCompEqual:        "==",
	OpCompNotEqual:     "!=",
	OpCompLess:         "<",
	OpCompLessEqual:    "<=",
	OpCompGreater:      ">",
	OpCompGreaterEqual: ">=",
	OpLogicalAnd:       "&&",
	OpLogicalOr:        "||",
	OpLogicalNot:       "!",
	OpAdd:              "+",
	OpSub:              "-",
	OpMult:             "*",
	OpDiv:              "/",
	OpMod:              "%",
	OpLikeRegex:        "like_regex",
}

type Operation struct {
	Type  OperationType
	Left  Path
	Right Path
}

var _ Path = Operation{}

func (o Operation) String() string {
	// TODO(normanchenn): Fix recursive brackets. When there is a operation like
	// 1 == 1 && 1 != 1, postgres will output (1 == 1 && 1 != 1), but we output
	// ((1 == 1) && (1 != 1)).
	if o.Type == OpLogicalNot {
		return fmt.Sprintf("%s(%s)", OperationTypeStrings[o.Type], o.Left)
	}
	return fmt.Sprintf("(%s %s %s)", o.Left, OperationTypeStrings[o.Type], o.Right)
}
