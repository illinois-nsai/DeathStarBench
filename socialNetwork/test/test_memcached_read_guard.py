"""Compile the production guard against a single-client pool test double."""
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


SOURCE = Path(__file__).resolve().parents[1] / "src"
MEMCACHED_API = """#pragma once
enum memcached_return_t { MEMCACHED_SUCCESS, MEMCACHED_FAILURE, MEMCACHED_END, MEMCACHED_TIMEOUT };
struct memcached_st {
  bool available = true;
  bool connected = false;
  bool unread_response = false;
  int connections = 0;
  int closes = 0;
  int returns = 0;
};
struct memcached_pool_st { memcached_st client; };
void memcached_quit(memcached_st *client);
memcached_return_t memcached_pool_push(memcached_pool_st *pool, memcached_st *client);
"""

CHECKS = r"""
#include <cassert>
#include <stdexcept>
#include <string>
#include <type_traits>
#include "MemcachedReadGuard.h"

using social_network::MemcachedReadGuard;
static_assert(!std::is_copy_constructible<MemcachedReadGuard>::value, "A lease has one owner");

memcached_st *borrow(memcached_pool_st &pool) {
  assert(pool.client.available);
  pool.client.available = false;
  if (!pool.client.connected) {
    pool.client.connected = true;
    ++pool.client.connections;
  }
  pool.client.unread_response = true;
  return &pool.client;
}

void memcached_quit(memcached_st *client) {
  assert(!client->available);
  client->connected = false;
  client->unread_response = false;
  ++client->closes;
}

memcached_return_t memcached_pool_push(memcached_pool_st *pool, memcached_st *client) {
  assert(client == &pool->client);
  assert(!client->available);
  assert(!client->unread_response);
  client->available = true;
  ++client->returns;
  return MEMCACHED_SUCCESS;
}

void completed_reads_reuse_connection() {
  memcached_pool_st pool;
  for (int request = 0; request < 100; ++request) {
    auto *client = borrow(pool);
    MemcachedReadGuard guard(&pool, client);
    client->unread_response = false;
    guard.Release(MEMCACHED_END);
  }
  assert(pool.client.connections == 1);
  assert(pool.client.closes == 0);
  assert(pool.client.returns == 100);
}

void incomplete_reads_reset_before_return() {
  for (auto status : {MEMCACHED_FAILURE, MEMCACHED_TIMEOUT, MEMCACHED_SUCCESS}) {
    memcached_pool_st pool;
    {
      MemcachedReadGuard guard(&pool, borrow(pool));
      guard.Release(status);
    }
    assert(pool.client.closes == 1);
    assert(pool.client.returns == 1);
    auto *client = borrow(pool);
    MemcachedReadGuard next_read(&pool, client);
    client->unread_response = false;
    next_read.Release(MEMCACHED_END);
    assert(pool.client.connections == 2);
  }
}

void decode_exception_releases_pool_slot() {
  memcached_pool_st pool;
  try {
    MemcachedReadGuard guard(&pool, borrow(pool));
    throw std::invalid_argument("invalid cached JSON or user ID");
  } catch (const std::invalid_argument &) {
  }
  assert(pool.client.available);
  assert(pool.client.closes == 1);
  assert(pool.client.returns == 1);
}

void release_precedes_database_fallback_and_refill() {
  memcached_pool_st pool;
  {
    auto *client = borrow(pool);
    MemcachedReadGuard read(&pool, client);
    client->unread_response = false;  // Cache misses also finish the response.
    read.Release(MEMCACHED_END);
    {
      auto *refill_client = borrow(pool);
      MemcachedReadGuard refill(&pool, refill_client);
      refill_client->unread_response = false;
      refill.Release(MEMCACHED_END);
    }
    read.Release(MEMCACHED_END);  // Explicit release and destruction are idempotent.
  }
  assert(pool.client.connections == 1);
  assert(pool.client.closes == 0);
  assert(pool.client.returns == 2);
}

int main(int argc, char **argv) {
  assert(argc == 2);
  const std::string name = argv[1];
  if (name == "reuse") completed_reads_reuse_connection();
  else if (name == "incomplete") incomplete_reads_reset_before_return();
  else if (name == "decode") decode_exception_releases_pool_slot();
  else if (name == "refill") release_precedes_database_fallback_and_refill();
  else return 1;
}
"""


class MemcachedReadGuardTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        compiler = shutil.which("c++")
        if compiler is None:
            raise unittest.SkipTest("A C++14 compiler is required")
        cls.temporary = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.temporary.cleanup)
        root = Path(cls.temporary.name)
        headers = root / "libmemcached"
        headers.mkdir()
        (headers / "memcached.h").write_text(MEMCACHED_API)
        (headers / "util.h").write_text('#include "memcached.h"\n')
        source = root / "test.cpp"
        source.write_text(CHECKS)
        cls.binary = root / "test"
        subprocess.run([compiler, "-std=c++14", "-Wall", "-Wextra", "-Werror", "-pedantic",
                        "-I", str(root), "-I", str(SOURCE), str(source), "-o", str(cls.binary)], check=True)

    def test_repeated_successful_reads_keep_one_connection(self):
        subprocess.run([self.binary, "reuse"], check=True)

    def test_errors_and_unfinished_reads_reset_connection(self):
        subprocess.run([self.binary, "incomplete"], check=True)

    def test_decoding_exception_returns_the_borrowed_client(self):
        subprocess.run([self.binary, "decode"], check=True)

    def test_single_slot_pool_can_refill_before_guard_destruction(self):
        subprocess.run([self.binary, "refill"], check=True)


if __name__ == "__main__":
    unittest.main()
