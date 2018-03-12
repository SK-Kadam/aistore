DFC: Distributed File Cache with Amazon and Google Cloud backends
-----------------------------------------------------------------

## Overview

DFC is a simple distributed caching service written in Go. The service
currently consists of a single http proxy (with a well-known address)
and an arbitrary number of storage targets (aka targets):

<img src="images/dfc-overview.png" alt="DFC overview" width="440">

Users (i.e., http/https clients) connect to the proxy and execute RESTful
commands. Data then moves directly between storage targets (that cache this data)
and the requesting user.

## Getting Started

If you've already installed Go and happen to have AWS or GCP account, getting
started with DFC takes about 30 seconds and consists in executing the following
4 steps:

```
$ go get -u -v github.com/NVIDIA/dfcpub/dfc
$ cd $GOPATH/src/github.com/NVIDIA/dfcpub/dfc
$ make deploy
$ go test ./tests -v -run=down -numfiles=2 -bucket=<your bucket name>
```

The 1st command will install both the DFC source code and all its dependencies
under your configured $GOPATH.

The 3rd - deploys DFC daemons locally (for details, please see [the script](dfc/setup/deploy.sh)).

Finally, the 4th command executes a smoke test to download 2 (two) files
from your own named Amazon S3 or Google Cloud Storage bucket. Here's another example:

```
$ go test ./tests -v -run=download -args -numfiles=100 -match='a\d+' -bucket=myS3bucket
```
This will download up to 100 objects from the bucket called myS3bucket.
The names of those objects will match 'a\d+' regex.

For more testing/running command line options, please refer to [the source](dfc/tests/main_test.go).

For other useful commands, see the [Makefile](dfc/Makefile).

## Helpful Links: Go

* [How to write Go code](https://golang.org/doc/code.html)

* [How to install Go binaries and tools](https://golang.org/doc/install)

* [The Go Playground](https://play.golang.org/)

* [Go language support for Vim](https://github.com/fatih/vim-go)
  (note: if you are a VIM user vim-go plugin is invaluable)

* [Go lint tools to check Go source for errors and warnings](https://github.com/alecthomas/gometalinter)

## Helpful Links: AWS

* [AWS Command Line Interface](https://docs.aws.amazon.com/cli/latest/userguide/cli-chap-getting-started.html)

* [AWS S3 Tutorial For Beginners](https://www.youtube.com/watch?v=LfBn5Y1X0vE)


## Configuration

DFC configuration is consolidated in a single [JSON file](dfc/setup/config.sh) where all of the knobs must be self-explanatory and the majority of those, except maybe just a few, have pre-assigned default values. The notable exceptions include:

<img src="images/dfc-config-1.png" alt="DFC configuration: TCP port and URL" width="512">

and

<img src="images/dfc-config-2-commented.png" alt="DFC configuration: local filesystems" width="548">

## Miscellaneous

The following sequence downloads 100 objects from the bucket "myS3bucket", and then
finds the corresponding cached files and generated logs:
```
$ go test -v -run=down -bucket=myS3bucket
$ find /tmp/dfc/ -type f | grep cache
$ find /tmp/dfc/ -type f | grep log
```

Don't forget, though, to run 'make deploy' first.

To terminate a running DFC service and cleanup local caches, run:
```
$ make kill
$ make rmcache
```
## REST operations

DFC supports a growing number and variety of RESTful operations. To illustrate common conventions, let's take a look at the example:

```
$ curl -X GET -H 'Content-Type: application/json' -d '{"what": "config"}' http://192.168.176.128:8080/v1/cluster
```

This command queries the DFC configuration; at the time of this writing it'll result in a JSON output that looks as follows:

> {"smap":{"":{"node_ip_addr":"","daemon_port":"","daemon_id":"","direct_url":""},"15205:8081":{"node_ip_addr":"192.168.176.128","daemon_port":"8081","daemon_id":"15205:8081","direct_url":"http://192.168.176.128:8081"},"15205:8082":{"node_ip_addr":"192.168.176.128","daemon_port":"8082","daemon_id":"15205:8082","direct_url":"http://192.168.176.128:8082"},"15205:8083":{"node_ip_addr":"192.168.176.128","daemon_port":"8083","daemon_id":"15205:8083","direct_url":"http://192.168.176.128:8083"}},"version":5}

Notice the 4 (four) ubiquitous elements in the `curl` command line above:

1. HTTP verb aka method.

In the example, it's a GET but it can also be POST, PUT, and DELETE. For a brief summary of the standard HTTP verbs and their CRUD semantics, see, for instance, this [REST API tutorial](http://www.restapitutorial.com/lessons/httpmethods.html).

2. URL path: hostname or IP address of one of the DFC servers.

By convention, REST operation implies a "clustered" scope when performed on a DFC proxy server.

3. URL path: version of the REST API, resource that is operated upon, and possibly more forward-slash delimited specifiers.

For example: /v1/cluster where 'v1' is the currently supported version and 'cluster' is the resource.

4. Control message in JSON format, e.g. `{"what": "config"}`.

> Combined, all these elements tell the following story. They tell us what to do in the most generic terms (e.g., GET), designate the target aka "resource" (e.g., cluster), and may also include context-specific and JSON-encoded control message to, for instance, distinguish between getting system statistics (`{"what": "stats"}`) versus system configuration (`{"what": "config"}`).

| Operation | HTTP action | Example |
|--- | --- | ---|
| Unregister storage target (proxy only) | DELETE /v1/cluster/daemon/daemonID | `curl -i -X DELETE http://192.168.176.128:8080/v1/cluster/daemon/15205:8083` |
| Register storage target | POST /v1//daemon | `curl -i -X POST http://192.168.176.128:8083/v1/daemon` |
| Get cluster map (proxy only) | GET {"what": "smap"} /v1/cluster | `curl -X GET -H 'Content-Type: application/json' -d '{"what": "smap"}' http://192.168.176.128:8080/v1/cluster` |
| Get proxy or target configuration| GET {"what": "config"} /v1/daemon | `curl -X GET -H 'Content-Type: application/json' -d '{"what": "config"}' http://192.168.176.128:8080/v1/daemon` |
| Set proxy or target configuration | PUT {"action": "setconfig", "name": "some-name", "value": "other-value"} /v1/daemon | `curl -i -X PUT -H 'Content-Type: application/json' -d '{"action": "setconfig","name": "stats_time", "value": "1s"}' http://192.168.176.128:8081/v1/daemon` |
| Set cluster configuration  (proxy only) | PUT {"action": "setconfig", "name": "some-name", "value": "other-value"} /v1/cluster | `curl -i -X PUT -H 'Content-Type: application/json' -d '{"action": "setconfig","name": "stats_time", "value": "1s"}' http://192.168.176.128:8080/v1/cluster` |
| Shutdown target | PUT {"action": "shutdown"} /v1/daemon | `curl -i -X PUT -H 'Content-Type: application/json' -d '{"action": "shutdown"}' http://192.168.176.128:8082/v1/daemon` |
| Shutdown cluster (proxy only) | PUT {"action": "shutdown"} /v1/cluster | `curl -i -X PUT -H 'Content-Type: application/json' -d '{"action": "shutdown"}' http://192.168.176.128:8080/v1/cluster` |
| Rebalance cluster (proxy only) | PUT {"action": "rebalance"} /v1/cluster | `curl -i -X PUT -H 'Content-Type: application/json' -d '{"action": "rebalance"}' http://192.168.176.128:8080/v1/cluster` |
| Get cluster statistics (proxy only) | GET {"what": "stats"} /v1/cluster | `curl -X GET -H 'Content-Type: application/json' -d '{"what": "stats"}' http://192.168.176.128:8080/v1/cluster` |
| Get target statistics | GET {"what": "stats"} /v1/daemon | `curl -X GET -H 'Content-Type: application/json' -d '{"what": "stats"}' http://192.168.176.128:8083/v1/daemon` |
| Get object (proxy only) | GET /v1/files/bucket/object | `curl -L -X GET http://192.168.176.128:8080/v1/files/myS3bucket/myS3object -o myS3object` (`*`) |
| List bucket | GET { properties-and-options... } /v1/files/bucket | `curl -X GET -L -H 'Content-Type: application/json' -d '{"props": "size"}' http://192.168.176.128:8080/v1/files/myS3bucket` (`**`) |
| Rename/move file (local buckets only) | POST {"action": "rename", "name": new-name} /v1/files/bucket | `curl -i -X POST -L -H 'Content-Type: application/json' -d '{"action": "rename", "name": "dir2/DDDDDD"}' http://192.168.176.128:8080/v1/files/mylocalbucket/dir1/CCCCCC` (`***`)|
| Copy file | PUT /v1/files/from_id/to_id/bucket/object | `curl -i -X PUT http://192.168.176.128:8083/v1/files/from_id/15205:8083/to_id/15205:8081/myS3bucket/myS3object` (`****`) |
| Delete file | DELETE /v1/files/bucket/object | `curl -i -X DELETE -L http://192.168.176.128:8080/v1/files/mybucket/mydirectory/myobject` |
| Evict file from cache | DELETE '{"action": "evict"}' /v1/files/bucket/object | `curl -i -X DELETE -L -H 'Content-Type: application/json' -d '{"action": "evict"}' http://192.168.176.128:8080/v1/files/mybucket/myobject` |
| Create local bucket (proxy only) | POST {"action": "createlb"} /v1/files/bucket | `curl -i -X POST -H 'Content-Type: application/json' -d '{"action": "createlb"}' http://192.168.176.128:8080/v1/files/abc` |
| Destroy local bucket (proxy only) | DELETE /v1/files/bucket | `curl -i -X DELETE http://192.168.176.128:8080/v1/files/abc` |
| Prefetch a list of objects | POST '{"action":"prefetch", "value":{"objnames":"[o1[,o]*]"[, deadline: string][, wait: bool]}}' /v1/files/bucket | `curl -i -X POST -H 'Content-Type: application/json' -d '{"action":"prefetch", "value":{"objnames":["o1","o2","o3"], "deadline": "10s", "wait":true}}' http://192.168.176.128:8080/v1/files/abc` (`*****`) |
| Prefetch a range of objects| POST '{"action":"prefetch", "value":{"prefix":"your-prefix","regex":"your-regex","range","min:max" [, deadline: string][, wait:bool]}}' /v1/files/bucket | `curl -i -X POST -H 'Content-Type: application/json' -d '{"action":"prefetch", "value":{"prefix":"__tst/test-", "regex":"\\d22\\d", "range":"1000:2000", "deadline": "10s", "wait":true}}' http://192.168.176.128:8080/v1/files/abc` (`*****`) |
| Get bucket props (local and cloud) | HEAD /v1/files/bucket | ``` curl --head http://192.168.176.128:8080/v1/files/abc ```|

> (`*`) This will fetch the object "myS3object" from the bucket "myS3bucket". Notice the -L - this option must be used in all DFC supported commands that read or write data - usually via the URL path /v1/files/. For more on the -L and other useful options, see [Everything curl: HTTP redirect](https://ec.haxx.se/http-redirects.html).

> (`**`) See the List Bucket section for details.

> (`***`) Notice the -L option here and elsewhere.

> (`****`) Advanced usage only.

> (`*****`) See the Prefetching section for details.

### Example: querying runtime statistics

```
$ curl -X GET -H 'Content-Type: application/json' -d '{"what": "stats"}' http://192.168.176.128:8080/v1/cluster
```

This single command causes execution of multiple `GET {"what": "stats"}` requests within the DFC cluster, and results in a JSON-formatted consolidated output that contains both http proxy and storage targets request counters, as well as per-target used/available capacities. For example:

<img src="images/dfc-get-stats.png" alt="DFC statistics" width="440">

More usage examples can be found in the [the source](dfc/tests/regression_test.go).

## List Bucket

the ListBucket API returns a page of up to 1000 object names (and, optionally, their properties including sizes, creation times, checksums, and more), in addition to a token allowing the next page to be retrieved.

### properties-and-options
The properties-and-options specifier must be a JSON-encoded structure, for instance '{"props": "size"}' (see examples). An empty structure '{}' results in getting just the names of the objects (from the specified bucket) with no other metadata.

| Property/Option | Meaning | Value |
| --- | --- | --- |
| props | The properties to return with object names | A comma-separated string containing any combination of: "checksum","size","atime","ctime","iscached","bucket","version". |
| time_format | The standard by which times should be formatted | Any of the following [golang time constants](http://golang.org/pkg/time/#pkg-constants): RFC822, Stamp, StampMilli, RFC822Z, RFC1123, RFC1123Z, RFC3339. The default is RFC822. |
| prefix | The prefix which all returned objects must have. | For example, "my/directory/structure/" |
| pagemarker | The token signifying the next page to retrieve | Returned in the "nextpage" field from a call to ListBucket that does not retrieve all keys. When the last key is retrieved, NextPage will be the empty string |\b

### Example: listing local and Cloud buckets

To list objects in the smoke/ subdirectory of a given bucket called 'myBucket', and to include in the listing their respective sizes and checksums, run:

```
$ curl -X GET -L -H 'Content-Type: application/json' -d '{"props": "size, checksum", "prefix": "smoke/"}' http://192.168.176.128:8080/v1/files/myBucket
```

This request will produce an output that (in part) may look as follows:

<img src="images/dfc-ls-subdir.png" alt="DFC list directory" width="440">

For many more examples, please refer to the dfc/tests/*_test.go files in the repository.

### Example: Listing All Pages

The following Go code retrieves a list of all of the keys in a bucket (Error handling omitted).

```go
// proxyurl, bucket are your DFC Proxy URL and your bucket.
url := proxyurl + "/v1/files/" + bucket

msg := &dfc.GetMsg{}
fullbucketlist := &dfc.BucketList{Entries: make([]*dfc.BucketEntry, 0)}
for {
    // 1. First, send the request
    jsbytes, _ := json.Marshal(msg)
    request, _ := http.NewRequest("GET", url, bytes.NewBuffer(jsbytes))
    r, _ := http.DefaultClient.Do(request)
    defer func(r *http.Response){
        r.Body.Close()
    }(r)

    // 2. Unmarshal the response
    pagelist := &dfc.BucketList{}
    respbytes, _ := ioutil.ReadAll(r.Body)
    _ = json.Unmarshal(respbytes, pagelist)

    // 3. Add the entries to the list
    fullbucketlist.Entries = append(fullbucketlist.Entries, pagelist.Entries...)
    if pagelist.PageMarker == "" {
        // If PageMarker is the empty string, this was the last page
        break
    }
    // If not, update PageMarker to the next page returned from the request.
    msg.GetPageMarker = pagelist.PageMarker
}
```

Note that the PageMarker returned as a part of pagelist is for the next page.

## Cache Rebalancing

DFC rebalances its cached content based on the DFC cluster map. When cache servers join or leave the cluster, the next updated version (aka generation) of the cluster map gets centrally replicated to all storage targets. Each target then starts, in parallel, a background thread to traverse its local caches and recompute locations of the cached items.

Thus, the rebalancing process is completely decentralized. When a single server joins (or goes down in a) cluster of N servers, approximately 1/Nth of the content will get rebalanced via direct target-to-target transfers.

## Prefetching

DFC provides an API to prefetch objects (cache objects on the DFC server without retrieving them through a GET request). There are two forms of this API: List, and Range. Both of these share two optional parameters:

| Parameter | Meaning | Default |
|--- | --- | --- |
| deadline | The amount of time before the prefetch request expires formatted as a [golang duration string](https://golang.org/pkg/time/#ParseDuration). A timeout of 0 means no timeout.| 0 |
| wait | If true, a response will be sent only once all requested files have been prefetched or the deadline passes. When false, a response will be sent once the prefetch request is initiated. When setting wait=true, ensure your request has a timeout at least as long as the deadline. | false |

### List

List Prefetch takes a JSON array of object names to prefetch, and initiates a prefetch request for those objects.

| Parameter | Meaning |
| --- | --- |
| objnames | The JSON array of object names to be prefetched. |

### Range

Range Prefetch takes a prefix, a regular expression, and a range, and prefetches all files beginning with the prefix where a number matching the regular expression immediately follows and is within the range.


| Parameter | Meaning |
| --- | --- |
| prefix | The prefix that all prefetched files will begin with. |
| regex | The regular expression to match to the number following the prefix, represented as an escaped string. If it is an empty string (""), it will match all files. |
| range | Represented as "min:max", corresponding to the inclusive range from min to max. Range may be empty (""), meaning it will return all matches from the regex. If regex is the empty string, range will be ignored. |

#### Examples
| Prefix | Regex |  Escaped Regex | Range | Matches | Doesn't Match |
| --- | --- | --- | --- | --- | --- |
| "__tst/test-" | `"\d22\d"` | `"\\d22\\d"` | "1000:2000" | "__tst/test-1223","__tst/test-1229-4000.dat" | "__prod/test-1223", "__tst/test-1333", "__tst/test-12222-40000.dat", "__tst/test-2222-4000.dat" |
| "a/b/c" | `"\d+1\d"` | `"\\d+1\\d"` | "0:100000" | "a/b/c/110", "a/b/c/99919-200000.dat", "a/b/c/2314video-big" | "a/b/110", "a/b/c/d/110", "a/b/c/video-99919-20000.dat", "a/b/c/100012", "a/b/c/30111" |
