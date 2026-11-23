// Regression for connecting/authenticating while holding the Redis pool mutex.
// Uses only loopback and a small fake RESP AUTH server; no Redis deployment needed.
#include <sw/redis++/connection_pool.h>
#include <arpa/inet.h>
#include <sys/socket.h>
#include <unistd.h>
#include <atomic>
#include <chrono>
#include <cstring>
#include <future>
#include <iostream>
#include <stdexcept>
#include <thread>
#include <vector>

using namespace std::chrono;

struct FakeRedis {
  int listener;
  int port;
  int slow_index;
  bool fail_first;
  std::atomic<bool> stop{false};
  std::atomic<int> accepted{0}, delayed{0};
  std::thread acceptor;
  std::vector<std::thread> workers;

  FakeRedis(int slow, bool fail = false) : slow_index(slow), fail_first(fail) {
    listener = socket(AF_INET, SOCK_STREAM, 0);
    sockaddr_in address{};
    address.sin_family = AF_INET;
    address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    if (bind(listener, reinterpret_cast<sockaddr*>(&address), sizeof(address)) != 0
        || listen(listener, 8) != 0) throw std::runtime_error("listen failed");
    socklen_t length = sizeof(address);
    getsockname(listener, reinterpret_cast<sockaddr*>(&address), &length);
    port = ntohs(address.sin_port);
    acceptor = std::thread([this] {
      while (!stop) {
        int fd = accept(listener, nullptr, nullptr);
        if (fd < 0) break;
        int index = ++accepted;
        workers.emplace_back([this, fd, index] {
          timeval timeout{0, 100000};
          setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
          char buffer[1024];
          int n;
          do { n = recv(fd, buffer, sizeof(buffer), 0); } while (n < 0 && !stop);
          if (n > 0) {
            if (index == slow_index) {
              delayed = index;
              std::this_thread::sleep_for(milliseconds(400));
            }
            const char* reply = fail_first && index == 1 ? "-ERR auth failed\r\n" : "+OK\r\n";
            send(fd, reply, strlen(reply), MSG_NOSIGNAL);
            while (!stop && recv(fd, buffer, sizeof(buffer), 0) != 0) {}
          }
          close(fd);
        });
      }
    });
  }
  ~FakeRedis() {
    stop = true;
    shutdown(listener, SHUT_RDWR);
    close(listener);
    acceptor.join();
    for (auto& t : workers) t.join();
  }
  void wait_for_delay() {
    auto end = steady_clock::now() + seconds(3);
    while (!delayed && steady_clock::now() < end) std::this_thread::sleep_for(milliseconds(1));
    if (!delayed) throw std::runtime_error("probe server did not receive AUTH");
  }
};

sw::redis::ConnectionOptions options(const FakeRedis& server) {
  sw::redis::ConnectionOptions opts;
  opts.host = "127.0.0.1";
  opts.port = server.port;
  opts.password = "probe";
  opts.connect_timeout = milliseconds(1000);
  opts.socket_timeout = milliseconds(2000);
  return opts;
}

int main(int argc, char**) {
  try {
    bool baseline = argc > 1;
    long release_ms;
    {
      FakeRedis server(2);
      sw::redis::ConnectionPoolOptions pool_opts;
      pool_opts.size = 2;
      pool_opts.wait_timeout = milliseconds(1000);
      sw::redis::ConnectionPool pool(pool_opts, options(server));
      auto first = pool.fetch();
      auto future = std::async(std::launch::async, [&] {
        auto next = pool.fetch();
        pool.release(std::move(next));
      });
      server.wait_for_delay();
      auto start = steady_clock::now();
      pool.release(std::move(first));
      release_ms = duration_cast<milliseconds>(steady_clock::now() - start).count();
      future.get();
      if (!baseline && release_ms >= 100) throw std::runtime_error("returning healthy client blocked behind AUTH");
      if (server.accepted != 2) throw std::runtime_error("unexpected pool growth");
    }
    {
      FakeRedis server(1, true);
      sw::redis::ConnectionPoolOptions pool_opts;
      pool_opts.size = 1;
      pool_opts.wait_timeout = milliseconds(1000);
      sw::redis::ConnectionPool pool(pool_opts, options(server));
      auto first = std::async(std::launch::async, [&] {
        try { auto c = pool.fetch(); pool.release(std::move(c)); return false; }
        catch (const sw::redis::Error&) { return true; }
      });
      server.wait_for_delay();
      auto recovered = pool.fetch();
      pool.release(std::move(recovered));
      if (!first.get() || server.accepted != 2) throw std::runtime_error("failed connection reservation was not recovered");
    }
    std::cout << "{\"release_ms\":" << release_ms << ",\"failed_creation_recovered\":true}" << std::endl;
    return 0;
  } catch (const std::exception& e) {
    std::cerr << e.what() << std::endl;
    return 1;
  }
}
