# LogQL parser corpus

`TestLogQLCorpus` parses every expression in the `*.json` files here plus
every Loki expression in the lab (`testdata/lab`), prints the parse rate and
fails if it drops below `baseline`. Raise `baseline` when the rate improves;
never lower it to make a change pass.

There is no public, permissively licensed set of real LogQL to vendor (the
Loki mixins are AGPL-3.0, see ADR 0005), so this corpus grows from design
partners' queries. Until the first one, it holds only the lab.

## Adding a design partner's queries

1. Have the partner's written consent to capture fixtures (kept outside this
   repo).
2. Pick an anonymous name: `partner-a`, `partner-b`, and so on. Never put a
   company name, team name or anything that identifies the partner in the
   repo, including in file names and sources.
3. Collect their Loki rule files (`.yaml`, the ruler's format) and dashboard
   JSON, then run:

   ```sh
   go run ./pkg/parse/internal/corpusgen logql -source partner-a <files or dirs...> \
     > pkg/parse/testdata/logql-corpus/partner-a.json
   ```

   Rule expressions are kept only if they contain a stream selector (mixins
   keep Prometheus rules next to Loki rules). Dashboards keep panels whose
   datasource type is `loki`.

   Values are redacted: matcher values, label-filter values and line-filter
   text become `"redacted"` (Grafana variables in them are kept), and
   `ip(...)` ranges become `192.0.2.0/24`. Anything that decides references
   is kept: label names, `label_replace` arguments, `regexp` and `pattern`
   strings, templates (`{{ ... }}`) and `#` comments. `FuzzRedactLogQL`
   checks redaction never changes a query's references.

4. Read the output before committing. Kept strings and comments can still
   hold hostnames, customer names or other literal text; edit them out by
   hand (keep regexp group names and template fields).
5. Run `go test ./pkg/parse -run TestLogQLCorpus -v`. Fix extractor gaps it
   shows, or record the new rate in `baseline`.

## Checking against queries that can't be committed

`TestLogQLLocalCorpus` reads the files or directories in `TVT_LOGQL_CORPUS`
(separated like `PATH`), prints the parse rate and lists failures, without
copying anything into the repo. It is skipped unless the variable is set.
Use it for the Loki mixins or a partner's raw export before redaction:

```sh
TVT_LOGQL_CORPUS=~/src/loki/production/loki-mixin-compiled \
  go test ./pkg/parse -run TestLogQLLocalCorpus -v
```

`TVT_LOGQL_CORPUS_MIN=0.95` makes a lower rate fail.
