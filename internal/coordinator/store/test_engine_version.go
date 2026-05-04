package store

// TestEngineVersion mirrors engine.EngineVersion. The store
// package can't import engine without a cycle (engine imports
// store), so the constant is duplicated here.
//
// Lives in a non-_test file so external test packages
// (e.g. test/) can import it for drift detection — see
// test/TestEngineVersionsMatch, which fails the build if
// this falls out of sync with engine.EngineVersion.
//
// Production code must NOT read this — use engine.EngineVersion
// instead. The "Test" prefix in the name marks it as
// test-helper-only; the linter has no way to enforce that yet.
const TestEngineVersion = "j1.0"
