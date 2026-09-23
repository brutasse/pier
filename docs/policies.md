# Writing policy rules

`policy.rules` is an ordered list; the **first rule whose `when`
expression is true** allows the request, otherwise it is denied
(`default_deny: true`, the default). A rule that cannot be evaluated for
this token (a claim it references does not exist) simply does not match.

Variables available in expressions:

| Variable   | Meaning |
|------------|---------|
| `claims`   | map of the verified token's claims |
| `action`   | `"download"`, `"upload"` or `"delete"` |
| `group`    | Maven groupId (`com.example`), `""` for `maven-metadata.xml` |
| `artifact` | Maven artifactId |
| `version`  | Maven version, `""` for `maven-metadata.xml` |
| `snapshot` | `true` when the version ends with `-SNAPSHOT` |
| `path`     | full repository-relative path |

The `action:` field of a rule optionally limits it to specific actions
(empty = all actions). Rules compile at startup; a syntax error in a rule
prevents the service from starting.
