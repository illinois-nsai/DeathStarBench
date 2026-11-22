#ifndef SOCIAL_NETWORK_MICROSERVICES_MEMCACHEDREADGUARD_H_
#define SOCIAL_NETWORK_MICROSERVICES_MEMCACHEDREADGUARD_H_

#include <libmemcached/memcached.h>
#include <libmemcached/util.h>

namespace social_network {

// Return a borrowed multi-get client even when decoding a cached value throws.
class MemcachedReadGuard {
 public:
  MemcachedReadGuard(memcached_pool_st *pool, memcached_st *client)
      : _pool(pool), _client(client) {}

  MemcachedReadGuard(const MemcachedReadGuard &) = delete;
  MemcachedReadGuard &operator=(const MemcachedReadGuard &) = delete;

  ~MemcachedReadGuard() { Release(MEMCACHED_FAILURE); }

  void Release(memcached_return_t status) {
    if (!_client) {
      return;
    }
    // Only a fully consumed response can be reused by the next pool borrower.
    if (status != MEMCACHED_END) {
      memcached_quit(_client);
    }
    memcached_pool_push(_pool, _client);
    _client = nullptr;
  }

 private:
  memcached_pool_st *_pool;
  memcached_st *_client;
};

}  // namespace social_network

#endif  // SOCIAL_NETWORK_MICROSERVICES_MEMCACHEDREADGUARD_H_
