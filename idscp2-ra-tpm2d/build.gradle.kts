import com.google.protobuf.gradle.protobuf
import org.gradle.plugins.ide.idea.model.IdeaModel

apply(plugin = "java")
apply(plugin = "com.google.protobuf")
apply(plugin = "idea")

val generatedProtoBaseDir = "$projectDir/generated"

protobuf {
    generatedFilesBaseDir = generatedProtoBaseDir
}

tasks.named("clean") {
    doLast {
        delete(generatedProtoBaseDir)
    }
}

configure<IdeaModel> {
    module {
        // mark as generated sources for IDEA
        generatedSourceDirs.add(File("$generatedProtoBaseDir/main/java"))
    }
}

tasks.named("spotlessKotlin") {
    dependsOn(tasks.named("generateProto"))
    dependsOn(tasks.named("generateTestProto"))
}

val api by configurations
val testImplementation by configurations

dependencies {
    api("com.google.protobuf", "protobuf-java", "3.17.3")
    api("com.microsoft.azure", "TSS.Java", "1.0.0")

    testImplementation("org.awaitility", "awaitility-kotlin", "4.0.3")
    testImplementation("junit", "junit", "4.13.2")
    testImplementation("org.mockito", "mockito-core", "3.8.0")
}