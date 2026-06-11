// Package integration contains end-to-end tests that drive the
// onlineconf-admin docker-compose stack (admin server + MySQL seeded from
// admin/etc/*.sql) over its HTTP API.
//
// The tests are built only with the "integration" build tag and are excluded
// from regular unit-test runs (unit tests in turn carry the "!integration"
// constraint, so each tag selects exactly one suite):
//
//	go test ./...                                            # unit tests only
//	go test -tags integration -count=1 ./test/integration/   # integration tests only
//
// See README.md in this directory for details.
package integration
