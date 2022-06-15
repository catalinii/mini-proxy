# What is mini-proxy ?


It is a standalone http proxy or can be used as a peer to peer proxy.
It uses http2 connection between peers which is secure (using TLS) and reduces latency to remote locations.

# What features does it have ?
Aside from the standard features of a simple http proxy allowing CONNECT and GET/POST methods, it has the following additional features:
- creates a mesh network between each participants in the network
- simplifies the configuration of each peer by requiring only locally available domains + one peer
- will learn all the peers from the network and their defined destinations

# Any requirements ?
- it requires an publicly visible DNS name (eg. proxy.domain.com) pointing to a public ip address (ipv6 or ipv4) where the SSL port is accessible from the internet
- for IPv4 as long as the public port (sslport) is forwarded to the private IP, the application will work just fine. Ensure both the public and private port are the same.

# How is the configuration stored:
- it uses the proxy.yaml file in the current directory

# Description of the proxy.yaml configuration:
```
host: <public_hostname>
port: :8888
sslPort: :8889
allowedHosts:
    - 10.0.0.0/8
    - 192.168.0.0/16
    - 172.16.0.0/12
localDomains:
    - .*\.us$
peers:
  - https://remote_peer:8889
password: your_password
outgoingIp: local_ip
ipv6only: false
```
- host - mandatory - publically visible DNS name pointing to the local ip
- port - mandatory - http port for listening for proxy requests from clients
- sslPort - mandatory - port for connections from other peers (uses self generated TLS certificates)
- allowedHosts - mandatory - hosts that can connect without authentication at the http port (port configuration)
- localDomains - mandatory - domains that are advertised to the peers. All peers will use this proxy to connect to those domains (defined as regexp)
- peers - optional - list of remote peers. This host will initiate the connection to those peers to learn their local domains and list of peers
- password - optional - uses password to authenticate to the remote proxies. The password needs to be the same for all the participants in the mesh
- outgoingIp - optional - local ip used for the direct connections, that uses no other peer
- ipv6only - optional - uses ipv6 only to connect to the remote hosts.

# Example use case:
- Instance running in US having proxy.yaml:
```
host: <public_hostname_us>
port: :8888
sslport: :8889
allowedhosts:
    - 10.0.0.0/8
    - 192.168.0.0/16
    - 172.16.0.0/12
localdomains:
    - .*\.us$
peers:
  - https://remote_peer:8889
password: your_password
```
- Instance in Europe: 
```
host: <public_hostname_eu>
port: :8888
sslport: :8889
allowedhosts:
    - 10.0.0.0/8
    - 192.168.0.0/16
    - 172.16.0.0/12
localdomains:
    - .*\.eu$
password: your_password
```

- Connection from both peers to test.us will use the instance running in US
- Connection from both peers to europe.eu will use the instance running in EU
- Connection from each host to google.com will use their existing internet connection
