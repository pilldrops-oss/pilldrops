"""Fixtures for platform contract tests. Run: pytest tests/contract -v, or -m guard for the deploy-gate guards only.
Markers: guard (must pass, gates deploys), parity (encodes a known gap, expected to fail until fixed), slow (excluded from quick runs).
"""

import pytest

from pdlib import PDDriver, cli_authed


def pytest_configure(config):
    config.addinivalue_line("markers", "guard: regression guard, gates deploys")
    config.addinivalue_line("markers", "parity: encodes an open parity gap (fails until fixed)")
    config.addinivalue_line("markers", "slow: expensive test")


def pytest_collection_modifyitems(config, items):
    if not cli_authed():
        skip = pytest.mark.skip(
            reason="pilldrops CLI not authenticated — set PILLDROPS_API_KEY or run `pilldrops login`"
        )
        for item in items:
            item.add_marker(skip)


@pytest.fixture
def sandbox(request):
    """A default python-3.11 sandbox, destroyed after the test."""
    sb = PDDriver(template="python-3.11", timeout=600)
    request.addfinalizer(sb.destroy)
    return sb


@pytest.fixture
def sandbox_with_port(request):
    """A python-3.11 sandbox with port 8000 and a public preview, so data-plane guards get a tokenless URL."""
    sb = PDDriver(template="python-3.11", timeout=600, ports=[8000], public=True)
    request.addfinalizer(sb.destroy)
    return sb


@pytest.fixture
def sandbox_factory(request):
    """Create sandboxes with arbitrary options; all destroyed after the test."""
    created = []

    def make(**kwargs):
        kwargs.setdefault("timeout", 600)
        sb = PDDriver(**kwargs)
        created.append(sb)
        return sb

    def cleanup():
        for sb in created:
            sb.destroy()

    request.addfinalizer(cleanup)
    return make
