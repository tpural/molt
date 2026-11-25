plugins {
    java
    id("io.papermc.paperweight.userdev") version "2.0.0-beta.24"
}

group = "molt"
version = providers.gradleProperty("version").getOrElse("dev")

repositories {
    mavenCentral()
}

dependencies {
    paperweight.paperDevBundle("26.3.build.262-stable")
}

java {
    toolchain.languageVersion = JavaLanguageVersion.of(25)
}

tasks {
    processResources {
        filesMatching("paper-plugin.yml") {
            expand("version" to project.version)
        }
    }
    jar {
        archiveFileName = "molt-agent-${project.version}.jar"
    }
}
