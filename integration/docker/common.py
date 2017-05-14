import os
import string

import pytest

import cattle

ENV_MANAGER_IPS = "LONGHORN_MANAGER_TEST_SERVER_IPS"
ENV_BACKUPSTORE_URL = "LONGHORN_MANAGER_TEST_BACKUPSTORE_URL"
ENV_SYSLOG_TARGET = "LONGHORN_MANAGER_TEST_SYSLOG_TARGET"

MANAGER = 'http://localhost:9500'

SIZE = str(16 * 1024 * 1024)
VOLUME_NAME = "longhorn-manager-test_vol-1.0"
DEV_PATH = "/dev/longhorn/"

PORT = ":9500"


@pytest.fixture
def clients(request):
    ips = get_mgr_ips()
    client = get_client(ips[0] + PORT)
    setup_syslog(client)
    hosts = client.list_host()
    assert len(hosts) == len(ips)
    clis = get_clients(hosts)
    request.addfinalizer(lambda: cleanup_clients(clis))
    cleanup_clients(clis)
    return clis


def setup_syslog(client):
    setting = client.by_id_setting("syslogTarget")
    syslog_server = get_syslog_target()
    setting = client.update(setting, value=syslog_server)
    assert setting["value"] == syslog_server


def cleanup_clients(clis):
    client = clis.itervalues().next()
    volumes = client.list_volume()
    for v in volumes:
        client.delete(v)


def get_client(address):
    url = 'http://' + address + '/v1/schemas'
    c = cattle.from_env(url=url)
    return c


def get_mgr_ips():
    return string.split(os.environ[ENV_MANAGER_IPS], ",")


def get_backupstore_url():
    return os.environ[ENV_BACKUPSTORE_URL]


def get_syslog_target():
    return os.environ[ENV_SYSLOG_TARGET]


def get_clients(hosts):
    clients = {}
    for host in hosts:
        assert host["uuid"] is not None
        assert host["address"] is not None
        clients[host["uuid"]] = get_client(host["address"])
    return clients
