// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "AgentEngineHUD",
    platforms: [.macOS(.v13)],
    products: [.executable(name: "AgentEngineHUD", targets: ["AgentEngineHUD"])],
    targets: [
        .target(name: "AgentEngineHUDCore"),
        .executableTarget(name: "AgentEngineHUD", dependencies: ["AgentEngineHUDCore"]),
        .testTarget(name: "AgentEngineHUDCoreTests", dependencies: ["AgentEngineHUDCore"])
    ]
)
