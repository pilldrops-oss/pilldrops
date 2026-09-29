"""Driver smoke test: create → exec → file round-trip → destroy."""

import pytest

pytestmark = pytest.mark.guard


def test_smoke_create_exec_destroy(sandbox):
    r = sandbox.exec("echo contract-smoke-ok && python3 -V")
    assert r.exit_code == 0
    assert "contract-smoke-ok" in r.stdout
    assert "Python 3." in r.stdout


def test_smoke_file_roundtrip(sandbox):
    sandbox.write_file("/tmp/contract.txt", "hello-contract\n")
    assert "hello-contract" in sandbox.read_file("/tmp/contract.txt")
