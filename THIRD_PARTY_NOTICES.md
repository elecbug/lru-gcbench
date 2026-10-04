# License scope and third-party notices

The original lru-gcbench controller, measurement engine, adapters, reference
cache, tests, examples, and documentation are licensed under the MIT License.
Copyright (c) 2026 elecbug. See [LICENSE](LICENSE).

## google/go-lru

The separately maintained `github.com/google/go-lru` library retains its upstream
license and copyright notices. The checkout used for validation (commit
`6c2b8fa056eb77549eb8d2254a9940f52d92f9ff`) is licensed under the Apache License,
Version 2.0, with source notices identifying Copyright 2026 Google LLC.
The MIT license for lru-gcbench does not relicense google/go-lru.

- Upstream: https://github.com/google/go-lru
- Upstream license at the validated revision:
  https://github.com/google/go-lru/blob/6c2b8fa056eb77549eb8d2254a9940f52d92f9ff/LICENSE
- Included license copy: [licenses/google-go-lru-APACHE-2.0.txt](licenses/google-go-lru-APACHE-2.0.txt)

The builder compiles a worker using the user's local checkout; it does not copy
or modify that checkout's source or LICENSE. A compiled worker contains both
lru-gcbench and google/go-lru code. When distributing workers or third-party
source, retain the applicable upstream license and notices, including any NOTICE
file supplied by the particular checkout. Consult that checkout for its terms.

lru-gcbench is an independent project and is not an official Google product.
