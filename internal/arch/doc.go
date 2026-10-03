// Package arch holds the architecture tests of the module boundaries
// (docs/design/01 §5, 05 §2.4, §5.5; BE-004). It has no production code:
// the rules live in arch_test.go and run with make test.
//
//   - TestArch_LayerImports: a module imports only lower layers (01 §5.1);
//     platform and authz import no feature module; authztest is test-only.
//   - TestArch_TalentImportBan: only discovery, kvkk and cmd import talent (01 §5.4).
//   - TestArch_ForeignStoreBan: no module references another module's Store,
//     NewGormStore or NewMemStore (05 §2.4-1).
//   - TestArch_ScopeOrgRequired: every store_gorm.go method applies ScopeOrg
//     or documents "cross-org:" (05 §5.5).
//   - TestSyncWritesAreAllowed: synchronous cross-module writes are only the
//     ones of sync_writes.txt, the machine-readable 01 §6.3 (05 §2.4-3, 06 H-2).
//   - TestArch_GormParenParams: no GORM SQL string has a parameter directly
//     after "(" (06 H-30; BE-006).
//
// Every rule also runs on testdata/violations, a miniature tree that breaks
// it, so a rule that silently stops matching fails its own test.
package arch
