#!groovy
pipeline {
    agent none

    options {
        buildDiscarder(logRotator(daysToKeepStr: '30'))
        timeout(time: 3, unit: 'HOURS')
        timestamps()
    }
    parameters {
        booleanParam(name: 'unit', defaultValue: true, description: 'x86 unit tests')
        booleanParam(name: 'janky', defaultValue: true, description: 'x86 Build/Test')
        booleanParam(name: 'experimental', defaultValue: true, description: 'x86 Experimental Build/Test ')
        booleanParam(name: 'z', defaultValue: true, description: 'IBM Z (s390x) Build/Test')
        booleanParam(name: 'powerpc', defaultValue: true, description: 'PowerPC (ppc64le) Build/Test')
        booleanParam(name: 'vendor', defaultValue: true, description: 'Vendor')
        booleanParam(name: 'windowsRS1', defaultValue: false, description: 'Windows 2016 (RS1) Build/Test')
        booleanParam(name: 'windowsRS5', defaultValue: false, description: 'Windows 2019 (RS5) Build/Test')
        booleanParam(name: 'aarch64', defaultValue: true, description: 'ARM (aarch64) Build/Test')
    }
    environment {
        DOCKER_BUILDKIT = '1'
        APT_MIRROR      = 'cdn-fastly.deb.debian.org'
    }
    stages {
        stage('Build') {
            parallel {
                stage('unit') {
                    when {
                        beforeAgent true
                        expression { params.unit }
                    }
                    agent { label 'amd64 && ubuntu-1804 && overlay2' }

                    stages {
                        stage("Print info") {
                            steps {
                                sh 'docker version'
                                sh 'docker info'
                            }
                        }
                        stage("Build dev image") {
                            steps {
                                sh '''
                                # todo: include ip_vs in base image
                                sudo modprobe ip_vs
                
                                GITCOMMIT=$(git rev-parse --short HEAD)
                                docker build --force-rm --build-arg APT_MIRROR -t docker:$GITCOMMIT .
                                '''
                            }
                        }
                        stage("Run tests") {
                            steps {
                                sh '''
                                GITCOMMIT=$(git rev-parse --short HEAD)
               
                                docker run --rm -t --privileged \
                                  -v "$WORKSPACE/bundles:/go/src/github.com/docker/docker/bundles" \
                                  -v "$WORKSPACE/.git:/go/src/github.com/docker/docker/.git" \
                                  --name docker-pr$BUILD_NUMBER \
                                  -e DOCKER_GITCOMMIT=${GITCOMMIT} \
                                  -e DOCKER_GRAPHDRIVER=overlay2 \
                                  docker:$GITCOMMIT \
                                  hack/test/unit
                                '''
                            }
                        }
                    }

                    post {
                        always {
                            junit 'bundles/junit-report.xml'
                            sh '''
                            echo 'Ensuring container killed.'
                            docker rm -vf docker-pr$BUILD_NUMBER || true
                            '''

                            sh '''
                            echo 'Chowning /workspace to jenkins user'
                            docker run --rm -v "$WORKSPACE:/workspace" busybox chown -R "$(id -u):$(id -g)" /workspace
                            '''

                            sh '''
                            echo 'Creating unit-bundles.tar.gz'
                            tar -czvf unit-bundles.tar.gz bundles/junit-report.xml bundles/go-test-report.json bundles/profile.out
                            '''

                            archiveArtifacts artifacts: 'unit-bundles.tar.gz'
                        }
                        cleanup {
                            sh 'make clean'
                            deleteDir()
                        }
                    }
                }
                stage('janky') {
                    when {
                        beforeAgent true
                        expression { params.janky }
                    }
                    agent { label 'amd64 && ubuntu-1804 && overlay2' }

                    stages {
                        stage("Print info") {
                            steps {
                                sh 'docker version'
                                sh 'docker info'
                            }
                        }
                        stage("Build dev image") {
                            steps {
                                sh '''
                                # todo: include ip_vs in base image
                                sudo modprobe ip_vs
                
                                GITCOMMIT=$(git rev-parse --short HEAD)
                                docker build --force-rm --build-arg APT_MIRROR -t docker:$GITCOMMIT .
                                '''
                            }
                        }
                        stage("Run tests") {
                            steps {
                                sh '''
                                GITCOMMIT=$(git rev-parse --short HEAD)
        
                                docker run --rm -t --privileged \
                                  -v "$WORKSPACE/bundles:/go/src/github.com/docker/docker/bundles" \
                                  -v "$WORKSPACE/.git:/go/src/github.com/docker/docker/.git" \
                                  --name docker-pr$BUILD_NUMBER \
                                  -e DOCKER_GITCOMMIT=${GITCOMMIT} \
                                  -e DOCKER_GRAPHDRIVER=overlay2 \
                                  -e GIT_SHA1=${GIT_COMMIT} \
                                  docker:$GITCOMMIT \
                                  hack/ci/janky
                                '''
                            }
                        }
                        stage("Build e2e image") {
                            steps {
                                sh '''
                                GITCOMMIT=$(git rev-parse --short HEAD)
                                echo "Building e2e image"
                                docker build --build-arg DOCKER_GITCOMMIT=$GITCOMMIT -t moby-e2e-test -f Dockerfile.e2e .
                                '''
                            }
                        }
                    }

                    post {
                        always {
                            sh '''
                            echo "Ensuring container killed."
                            docker rm -vf docker-pr$BUILD_NUMBER || true
                            '''

                            sh '''
                            echo "Chowning /workspace to jenkins user"
                            docker run --rm -v "$WORKSPACE:/workspace" busybox chown -R "$(id -u):$(id -g)" /workspace
                            '''

                            sh '''
                            echo "Creating janky-bundles.tar.gz"
                            (find bundles -name '*.log' -o -name '*.prof' -o -name integration.test | xargs tar -czf janky-bundles.tar.gz) || true
                            '''

                            archiveArtifacts artifacts: 'janky-bundles.tar.gz'
                        }
                        cleanup {
                            sh 'make clean'
                            deleteDir()
                        }
                    }
                }
                stage('experimental') {
                    when {
                        beforeAgent true
                        expression { params.experimental }
                    }
                    agent { label 'amd64 && ubuntu-1804 && overlay2' }

                    stages {
                        stage("Print info") {
                            steps {
                                sh 'docker version'
                                sh 'docker info'
                            }
                        }
                        stage("Build dev image") {
                            steps {
                                sh '''
                                GITCOMMIT=$(git rev-parse --short HEAD)
                                docker build --force-rm --build-arg APT_MIRROR -t docker:${GITCOMMIT}-exp .
                                '''
                            }
                        }
                        stage("Run tests") {
                            steps {
                                sh '''
                                GITCOMMIT=$(git rev-parse --short HEAD)
                                docker run --rm -t --privileged \
                                    -v "$WORKSPACE/bundles:/go/src/github.com/docker/docker/bundles" \
                                    -e DOCKER_EXPERIMENTAL=y \
                                    --name docker-pr-exp$BUILD_NUMBER \
                                    -e DOCKER_GITCOMMIT=${GITCOMMIT} \
                                    -e DOCKER_GRAPHDRIVER=overlay2 \
                                    docker:${GITCOMMIT}-exp \
                                    hack/ci/experimental
                                '''
                            }
                        }
                    }

                    post {
                        always {
                            sh '''
                            echo "Ensuring container killed."
                            docker rm -vf docker-pr-exp$BUILD_NUMBER || true
                            '''

                            sh '''
                            echo "Chowning /workspace to jenkins user"
                            docker run --rm -v "$WORKSPACE:/workspace" busybox chown -R "$(id -u):$(id -g)" /workspace
                            '''

                            sh '''
                            echo "Creating bundles.tar.gz"
                            (find bundles -name '*.log' -o -name '*.prof' -o -name integration.test | xargs tar -czf experimental-bundles.tar.gz) || true
                            '''

                            archiveArtifacts artifacts: 'experimental-bundles.tar.gz'
                        }
                        cleanup {
                            sh 'make clean'
                            deleteDir()
                        }
                    }
                }
                stage('z') {
                    when {
                        beforeAgent true
                        expression { params.z }
                    }
                    agent { label 's390x-ubuntu-1604' }
                    // s390x machines run on Docker 18.06, and buildkit has some bugs on that version
                    environment { DOCKER_BUILDKIT = '0' }

                    stages {
                        stage("Print info") {
                            steps {
                                sh 'docker version'
                                sh 'docker info'
                            }
                        }
                        stage("Build dev image") {
                            steps {
                                sh '''
                                GITCOMMIT=$(git rev-parse --short HEAD)
                                docker build --force-rm --build-arg APT_MIRROR -t docker:$GITCOMMIT -f Dockerfile .
                                '''
                            }
                        }
                        stage("Run tests") {
                            steps {
                                sh '''
                                GITCOMMIT=$(git rev-parse --short HEAD)
                                docker run --rm -t --privileged \
                                  -v "$WORKSPACE/bundles:/go/src/github.com/docker/docker/bundles" \
                                    --name docker-pr$BUILD_NUMBER \
                                    -e DOCKER_GRAPHDRIVER=vfs \
                                    -e TIMEOUT="300m" \
                                    -e DOCKER_GITCOMMIT=${GITCOMMIT} \
                                    docker:$GITCOMMIT \
                                    hack/ci/z
                                '''
                            }
                        }
                    }

                    post {
                        always {
                            sh '''
                            echo "Ensuring container killed."
                            docker rm -vf docker-pr-$BUILD_NUMBER || true
                            '''

                            sh '''
                            echo "Chowning /workspace to jenkins user"
                            docker run --rm -v "$WORKSPACE:/workspace" busybox chown -R "$(id -u):$(id -g)" /workspace
                            '''

                            sh '''
                            echo "Creating bundles.tar.gz"
                            find bundles -name '*.log' | xargs tar -czf s390x-bundles.tar.gz
                            '''

                            archiveArtifacts artifacts: 's390x-bundles.tar.gz'
                        }
                        cleanup {
                            sh 'make clean'
                            deleteDir()
                        }
                    }
                }
                stage('powerpc') {
                    when {
                        beforeAgent true
                        expression { params.powerpc }
                    }
                    agent { label 'ppc64le-ubuntu-1604' }
                    // power machines run on Docker 18.06, and buildkit has some bugs on that version
                    environment { DOCKER_BUILDKIT = '0' }

                    stages {
                        stage("Print info") {
                            steps {
                                sh 'docker version'
                                sh 'docker info'
                            }
                        }
                        stage("Build dev image") {
                            steps {
                                sh '''
                                GITCOMMIT=$(git rev-parse --short HEAD)
                                docker build --force-rm --build-arg APT_MIRROR -t docker:$GITCOMMIT -f Dockerfile .
                                '''
                            }
                        }
                        stage("Run tests") {
                            steps {
                                sh '''
                                GITCOMMIT=$(git rev-parse --short HEAD)
                                docker run --rm -t --privileged \
                                  -v "$WORKSPACE/bundles:/go/src/github.com/docker/docker/bundles" \
                                    --name docker-pr$BUILD_NUMBER \
                                    -e DOCKER_GRAPHDRIVER=vfs \
                                    -e DOCKER_GITCOMMIT=${GITCOMMIT} \
                                    -e TIMEOUT="180m" \
                                    docker:$GITCOMMIT \
                                    hack/ci/powerpc
                                '''
                            }
                        }
                    }

                    post {
                        always {
                            sh '''
                            echo "Ensuring container killed."
                            docker rm -vf docker-pr$BUILD_NUMBER || true
                            '''

                            sh '''
                            echo "Chowning /workspace to jenkins user"
                            docker run --rm -v "$WORKSPACE:/workspace" busybox chown -R "$(id -u):$(id -g)" /workspace
                            '''

                            sh '''
                            echo "Creating bundles.tar.gz"
                            find bundles -name '*.log' | xargs tar -czf powerpc-bundles.tar.gz
                            '''

                            archiveArtifacts artifacts: 'powerpc-bundles.tar.gz'
                        }
                        cleanup {
                            sh 'make clean'
                            deleteDir()
                        }
                    }
                }
                stage('vendor') {
                    when {
                        beforeAgent true
                        expression { params.vendor }
                    }
                    agent { label 'amd64 && ubuntu-1804 && overlay2' }

                    stages {
                        stage("Print info") {
                            steps {
                                sh 'docker version'
                                sh 'docker info'
                            }
                        }
                        stage("Build dev image") {
                            steps {
                                sh '''
                                GITCOMMIT=$(git rev-parse --short HEAD)
                                docker build --force-rm --build-arg APT_MIRROR -t dockerven:$GITCOMMIT .
                                '''
                            }
                        }
                        stage("Run tests") {
                            steps {
                                sh '''
                                GITCOMMIT=$(git rev-parse --short HEAD)
                                docker run --rm -t --privileged \
                                  --name dockerven-pr$BUILD_NUMBER \
                                  -e DOCKER_GRAPHDRIVER=vfs \
                                  -v "$WORKSPACE/.git:/go/src/github.com/docker/docker/.git" \
                                  -e DOCKER_GITCOMMIT=${GITCOMMIT} \
                                  -e TIMEOUT=120m dockerven:$GITCOMMIT \
                                  hack/validate/vendor
                                '''
                            }
                        }
                    }
                    post {
                        cleanup {
                            sh 'make clean'
                            deleteDir()
                        }
                    }                }
                stage('windowsRS1') {
                    when {
                        beforeAgent true
                        expression { params.windowsRS1 }
                    }
                    agent {
                        node {
                            label 'windows-rs1'
                            customWorkspace 'c:\\gopath\\src\\github.com\\docker\\docker'
                        }
                    }
                    stages {
                        stage("Print info") {
                            steps {
                                sh 'docker version'
                                sh 'docker info'
                            }
                        }
                        stage("Run tests") {
                            steps {
                                powershell '''
                                $ErrorActionPreference = 'Stop'
                                .\\hack\\ci\\windows.ps1
                                exit $LastExitCode
                                '''
                            }
                        }
                    }
                }
                stage('windowsRS5-process') {
                    when {
                        beforeAgent true
                        expression { params.windowsRS5 }
                    }
                    agent {
                        node {
                            label 'windows-rs5'
                            customWorkspace 'c:\\gopath\\src\\github.com\\docker\\docker'
                        }
                    }
                    stages {
                        stage("Print info") {
                            steps {
                                sh 'docker version'
                                sh 'docker info'
                            }
                        }
                        stage("Run tests") {
                            steps {
                                powershell '''
                                $ErrorActionPreference = 'Stop'
                                .\\hack\\ci\\windows.ps1
                                exit $LastExitCode
                                '''
                            }
                        }
                    }
                }
            }
        }
        stage('aarch64') {
          when {
            beforeAgent true
            expression { params.aarch64 }
          }
          agent { label 'aarch64 && packet' }
          steps {
            sh '''
              GITCOMMIT=$(git rev-parse --short HEAD)

              docker build --rm --force-rm --build-arg APT_MIRROR=cdn-fastly.deb.debian.org -t docker-aarch64:$GITCOMMIT -f Dockerfile .

              docker run --rm -t --privileged \
                -v "$WORKSPACE/bundles:/go/src/github.com/docker/docker/bundles" \
                --name docker-pr-aarch64$BUILD_NUMBER \
                -e DOCKER_GRAPHDRIVER=vfs \
                -e DOCKER_EXECDRIVER=native \
                -e DOCKER_GITCOMMIT=${GITCOMMIT} \
                docker-aarch64:$GITCOMMIT \
                hack/ci/arm
            '''
          }
          post {
            always {
              sh '''
                echo "Ensuring container killed."
                docker rm -vf docker-pr-aarch64$BUILD_NUMBER || true

                echo "Chowning /workspace to jenkins user"
                docker run --rm -v "$WORKSPACE:/workspace" aarch64/busybox chown -R "$(id -u):$(id -g)" /workspace
              '''
              sh '''
                echo "Creating bundles.tar.gz"
                find bundles -name '*.log' | xargs tar -czf bundles.tar.gz
              '''
              archiveArtifacts artifacts: 'bundles.tar.gz'
            }
          }
        }
      }
    }
}
