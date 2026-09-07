// Package boundaries holds tests that assert architectural invariants rather
// than behaviour.
//
// It has no implementation on purpose. The invariants it guards are properties
// of the build graph, which no single package can check from the inside, and
// which are otherwise recorded only in prose that an added import silently
// invalidates.
package boundaries
