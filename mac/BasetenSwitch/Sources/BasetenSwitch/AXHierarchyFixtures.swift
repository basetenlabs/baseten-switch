#if DEBUG
import SwiftUI

enum AXHierarchyFixtureLevel: String {
    static let environmentKey = "BASETEN_SWITCH_AX_HIERARCHY_FIXTURE"

    case text
    case split
    case list
    case selection
    case scrollText
    case group
    case card
    case scroll
    case controls

    static var requested: AXHierarchyFixtureLevel? {
        ProcessInfo.processInfo.environment[environmentKey]
            .flatMap(AXHierarchyFixtureLevel.init(rawValue:))
    }
}

struct AXHierarchyFixtureView: View {
    let level: AXHierarchyFixtureLevel
    @State private var selection: String? = "overview"
    @State private var enabled = true
    @State private var rateLimitsEnabled = true
    @State private var serverErrorsEnabled = true
    @State private var model = "GLM-5.2"

    @ViewBuilder
    var body: some View {
        switch level {
        case .text:
            Text("Baseten Switch accessibility fixture")
                .frame(minWidth: 680, minHeight: 480)
        case .split:
            NavigationSplitView {
                Text("Sidebar")
            } detail: {
                Text("Detail")
            }
            .frame(minWidth: 680, minHeight: 480)
        case .list:
            NavigationSplitView {
                List {
                    Label("Overview", systemImage: "switch.2")
                    Section("Clients") {
                        Label("Claude Code", systemImage: "terminal")
                    }
                }
            } detail: {
                Text("Detail")
            }
            .frame(minWidth: 680, minHeight: 480)
        case .selection:
            NavigationSplitView {
                List(selection: $selection) {
                    Label("Overview", systemImage: "switch.2")
                        .tag("overview")
                    Section("Clients") {
                        Label("Claude Code", systemImage: "terminal")
                            .tag("claude-code")
                    }
                }
            } detail: {
                Text(selection == "claude-code" ? "Claude Code" : "Overview")
            }
            .frame(minWidth: 680, minHeight: 480)
        case .scrollText:
            NavigationSplitView {
                fixtureSidebar
            } detail: {
                ScrollView {
                    Text("Saved model mappings are active.")
                        .padding(28)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
            }
            .frame(minWidth: 680, minHeight: 480)
        case .group:
            NavigationSplitView {
                fixtureSidebar
            } detail: {
                GroupBox("Global Routing") {
                    Text("Saved model mappings are active.")
                        .padding()
                }
                .padding(28)
            }
            .frame(minWidth: 680, minHeight: 480)
        case .card:
            NavigationSplitView {
                fixtureSidebar
            } detail: {
                RoutingSectionCard {
                    Text("Global Routing")
                        .font(.headline)
                } content: {
                    Text("Saved model mappings are active.")
                        .padding()
                }
                .padding(28)
            }
            .frame(minWidth: 680, minHeight: 480)
        case .scroll:
            NavigationSplitView {
                List(selection: $selection) {
                    Label("Overview", systemImage: "switch.2")
                        .tag("overview")
                    Section("Clients") {
                        Label("Claude Code", systemImage: "terminal")
                            .tag("claude-code")
                    }
                }
            } detail: {
                ScrollView {
                    GroupBox("Global Routing") {
                        Text("Saved model mappings are active.")
                            .padding()
                    }
                    .padding(28)
                }
            }
            .frame(minWidth: 680, minHeight: 480)
        case .controls:
            NavigationSplitView {
                List(selection: $selection) {
                    Label("Overview", systemImage: "switch.2")
                        .tag("overview")
                    Section("Clients") {
                        Label("Claude Code", systemImage: "terminal")
                            .tag("claude-code")
                    }
                }
            } detail: {
                Form {
                    Toggle("Global Routing", isOn: $enabled)
                    Toggle(
                        "Rate limits (HTTP 429)",
                        isOn: $rateLimitsEnabled)
                        .accessibilityIdentifier("fallback-policy-429")
                    Toggle(
                        "Server errors (HTTP 5xx)",
                        isOn: $serverErrorsEnabled)
                        .accessibilityIdentifier("fallback-policy-5xx")
                    Picker("Model", selection: $model) {
                        Text("GLM-5.2").tag("GLM-5.2")
                        Text("Native Provider").tag("native")
                    }
                }
                .padding(28)
            }
            .frame(minWidth: 680, minHeight: 480)
        }
    }

    private var fixtureSidebar: some View {
        List(selection: $selection) {
            Label("Overview", systemImage: "switch.2")
                .tag("overview")
            Section("Clients") {
                Label("Claude Code", systemImage: "terminal")
                    .tag("claude-code")
            }
        }
    }
}
#endif
