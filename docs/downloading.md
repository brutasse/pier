# Downloading

## In CI (GitHub id-token)

Same mechanism as [publishing](publishing.md): request a token with your
repo's audience and pass it to the Maven resolver. For the Maven resolver
to send a custom header, use the wagon transport:

```sh
mvn -Dmaven.resolver.transport=wagon \
    -DremoteRepositories=pier::default::https://maven.example.com \
    dependency:get -Dartifact=com.example:myapp:1.2.3 -s settings.xml
```

The wagon flag can be set globally instead — see
[Enabling the wagon transport globally](publishing.md#enabling-the-wagon-transport-globally).

The matching rule (CI jobs of the project or its dependents may read):

```yaml
- name: ci-download
  action: [download]
  when: claims.repository in ["example/myproject", "example/consumer"]
```

## Individually-acquired tokens (humans, scripts)

Point at an external IdP in `auth.issuers` (its well-known URL), have
users request a token with your audience, and match on their claims:

```yaml
- name: staff-download
  action: [download]
  when: |
    claims.iss == "https://id.example.com" &&
    claims.sub in ["alice@example.com"]
```
