# Publishing from GitHub Actions

Trusted publishing: the job presents its own OIDC token, and the policy
binds the `repository` (and ref) claims to what may be published.

Workflow:

```yaml
permissions:
  contents: read
  id-token: write

jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-java@v6
        with: { distribution: temurin, java-version: "17" }
      - name: Request the OIDC token
        id: token
        uses: actions/github-script@v9
        with:
          script: |
            // audience must match config audiences
            core.setOutput('oidc', await core.getIDToken('https://maven.example.com'))
      - run: mvn -B deploy   # with the token attached, see below
```

The Maven deploy plugin does not natively send OIDC tokens. Two practical
options:

1. **Wagon custom headers** (simplest). Put the token into a Maven
   settings file and let the wagon HTTP transport send it:

   ```xml
   <!-- settings.xml -->
   <settings>
     <servers>
       <server>
         <id>pier</id>
         <configuration>
           <httpHeaders>
             <property>
               <name>Authorization</name>
               <value>Bearer ${env.PIER_TOKEN}</value>
             </property>
           </httpHeaders>
         </configuration>
       </server>
     </servers>
     <profiles>
       <profile>
         <id>pier</id>
         <repositories>
           <repository>
             <id>pier</id>
             <url>https://maven.example.com</url>
           </repository>
         </repositories>
       </profile>
     </profiles>
   </settings>
   ```

   ```sh
   export PIER_TOKEN="${{ steps.token.outputs.oidc }}"
   mvn -B -s settings.xml deploy:deploy-file \
     -Dfile=target/myapp.jar -DpomFile=pom.xml \
     -DgroupId=com.example -DartifactId=myapp -Dversion=1.2.3 \
     -Dpackaging=jar -DrepositoryId=pier \
     -Durl=https://maven.example.com
   ```

   A full `mvn deploy` lifecycle works the same way for the whole
   project (it also POSTs `maven-metadata.xml`).

2. **curl** — for scripts that already produce the files, `PUT` the jar,
   pom and checksums and `POST` the metadata; the deploy plugin is just
   doing that.

## Enabling the wagon transport globally

Maven 3.9+ uses the native HTTP resolver transport by default, which
does **not** send the wagon `httpHeaders` above — the token never
reaches pier and requests fail with 401. Instead of
`-Dmaven.resolver.transport=wagon` on every command, set the property
once:

1. **In the settings file** (recommended — it travels with the
   `httpHeaders` it enables). Put it in a profile that is activated
   *explicitly*: in the same file's `<activeProfiles>`, or with `-P`
   on the command line. Conditionally-activated profiles
   (`<activation>`, `<activeByDefault>`) are not considered — the
   resolver session, and with it the transport, is built before
   profile activation is evaluated:

   ```xml
   <profile>
     <id>pier</id>
     <properties>
       <maven.resolver.transport>wagon</maven.resolver.transport>
     </properties>
     <!-- ...repositories... -->
   </profile>

   <activeProfiles>
     <activeProfile>pier</activeProfile>
   </activeProfiles>
   ```

2. **Machine-wide**: the `mvn` launcher prepends `MAVEN_ARGS` to every
   invocation, so export it in your shell profile or the CI
   environment:

   ```sh
   export MAVEN_ARGS="-Dmaven.resolver.transport=wagon"
   ```

3. **Per project**: a `.mvn/maven.config` file in the repository (one
   option per line) applies to every Maven invocation run in that
   project:

   ```
   -Dmaven.resolver.transport=wagon
   ```

Example policy rule binding the GitHub repo to publication, tag-only:

```yaml
- name: publish-from-tags
  action: [upload]
  when: |
    claims.repository == "example/myproject" &&
    claims.sub.startsWith("repo:example/myproject:ref:refs/tags/v")
```

GitHub token claims usable in rules: `iss`, `sub`
(`repo:owner/name:ref:<ref>`), `repository`, `repository_owner`, `aud`,
plus any custom claims your workflow requested.
