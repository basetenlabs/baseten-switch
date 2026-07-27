// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "BasetenSwitch",
    platforms: [.macOS(.v13)],
    targets: [
        .executableTarget(name: "BasetenSwitch", path: "Sources/BasetenSwitch"),
        .testTarget(name: "BasetenSwitchTests", dependencies: ["BasetenSwitch"])
    ]
)
