# Configuration
By default mold looks for a .mold.yml configuration file at the root of your project.
This contains all the necessary information to perform your build.  A sample with comments
can be found in [testdata/mold1.yml](../testdata/mold1.yml).

The build configuration is broken up into the following sections:

- [Services](#services)
- [Build](#build)
- [Artifacts/Publish](#artifacts)

This also is representative of the lifecycle the build follows.  Each of the above
happen in sequential order.

All sections aside from `build` are optional.

### Example:
This example contains all supported options.  The `services` and `build` definitions are
identical.  Multiple services and builds can be defined for each of these sections.

    # Launch services needed for the build
    services:
        - image: elasticsearch
        - image: progrium/consul
          commands:
              - -server
              - -bootstrap

    # Perform 1 or more builds
    build:
        - image: golang:1.8.1
          workdir: /go/src/github.com/d3sw/mold
          environment:
              - TEST_ENV=test_env
          commands:
              - hostname
              - uname -a
              - make
	  ports:
	      - "8080:8080"

    # Build docker images
    artifacts:
        # Only publish the image on the following branches/tags. * can be used to
        # publish on all branches/tags
        publish:
            - master
        # Default registry to use if not specified. Blank uses docker hub
        registry: test.docker.registry
        images:
            - name: d3sw/mold-test
              dockerfile: testdata/Dockerfile
              registry:

## Services
Services is a list of containers that need to be started prior to the build.  These are
containers your build process needs to perform the build.  For example if you are running
tests that require elasticsearch, you would declare a elasticsearch container to run
in this section as shown in the above example.

Service containers are spun up prior to the code build.  They are accessed via their image name followed
by the project name.  In the above example if the project name is `foo` you can access the consul service
using the host `consul.foo` in your build container.

#### image
The image name of the service to start.  A vast list of public images can be found on
[Docker Hub](https://hub.docker.com).  Private images can also be specified.

#### commands
These are a list of commands passed as arguments to the service container.

## Build
Build contains a list of builds to perform. This is used to perform testing and/or building binaries.  
Each build will run its set of provided commands in the specified container.  Any failed
command will cause the build to fail.  This is the only required configuration needed
to run the build.

#### workdir
This is **path inside the container** where the project repository will be accessible (mounted).
It can be an path of your choosing.  In the above example the source repo for mold will
be available under `/go/src/github.com/d3sw/mold` inside the `golang:1.8.1` container.  Any
data written to this directory is later available in the artifacts stage.

#### image
This is the docker image name used to build/test code.  These are disposable and not used to generate the final
artifact.  Code is built using this image and the generated binaries or files are then used to
package the image as specified in the [artifacts](#Artifacts) configuration.

#### commands
These are the commands that will be run in the container to do testing and building.

#### cache
If set to true it caches the build image to be reused on the next run.  By default it is
set to false.

## Artifacts
Artifacts are docker images to be built **using the data available from the build step**.
This is accomplished by using the working directory as context to the docker image build
process.

Using the specified Dockerfile and name, images are built which may be published to a registry
based on conditional parameters.  This is the final product destined for production.  
These images would be very trimmed down and as minimalistic as possible specifically tailored
for the application.  

#### registry
This option sets the default registry for all images in the case where it is not supplied.
It defaults to [Docker Hub](https://hub.docker.com) if not specified.

#### publish
This option specifies which branches will trigger a push to the registry.  Both exact and regular expression matches are supported.

- `.*` For all branches/tags
- `[v].+` For a version tag
- Name of a branch/tag

#### images
A list of images to build.  Each image has the following options available:

- **name**: Specifies the name of the image (required)

- **dockerfile**: Relative path to the Dockerfile. (required) Information on how a
Dockerfile works can be found [here](https://docs.docker.com/engine/reference/builder/)

- **registry**: Registry to push to.  If not specified the default one is used.

- **tags**: A list of additional image tags to be applied.  The above mentioned environment variables are
available here to use.

- **context**: A folder in which image build is processed. If your Dockerfile is in subfolder you may add **context** to build in this subfolder instead of building in `.mold.yml` file location

### Environment Variables
The following environment variables are available in your builds as well as in `tags` in the `artifacts` section:

- APP_VERSION
- APP_VERSION_SHORT
- APP_COMMIT
- APP_COMMIT_INDEX
