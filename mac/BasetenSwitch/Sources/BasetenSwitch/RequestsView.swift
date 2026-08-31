import Foundation
import SwiftUI

struct RequestsView: View {
    @ObservedObject var store: RequestsStore

    private var items: [RequestItem] {
        (store.snapshot?.items ?? []).sorted {
            requestTimestamp($0.completedAt)
                > requestTimestamp($1.completedAt)
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            header
                .padding(.horizontal, 28)
                .padding(.top, 24)
                .padding(.bottom, 16)

            status
                .padding(.horizontal, 28)

            content
        }
        .frame(
            maxWidth: .infinity,
            maxHeight: .infinity,
            alignment: .topLeading)
        .accessibilityIdentifier("requests")
        .onAppear { store.start() }
        .onDisappear { store.stop() }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack(alignment: .firstTextBaseline) {
                Label("Requests", systemImage: "list.bullet.rectangle")
                    .font(.title2.weight(.semibold))
                Spacer()
                HStack(spacing: 5) {
                    Circle()
                        .fill(store.isStale ? Color.orange : Color.green)
                        .frame(width: 7, height: 7)
                    Text(store.isStale ? "Reconnecting" : "Live")
                }
                .font(.caption)
                .foregroundStyle(.secondary)
                .accessibilityElement(children: .combine)
                .accessibilityLabel(
                    store.isStale
                        ? "Live request updates reconnecting"
                        : "Live request updates active")
                Button {
                    store.refresh()
                } label: {
                    if store.isRefreshing {
                        ProgressView()
                            .controlSize(.small)
                    } else {
                        Image(systemName: "arrow.clockwise")
                    }
                }
                .buttonStyle(.borderless)
                .disabled(
                    store.isLoading
                        || store.isRefreshing
                        || store.isLoadingOlder)
                .help("Refresh requests")
                .accessibilityLabel("Refresh requests")
            }

            Picker("Request filter", selection: $store.filter) {
                ForEach(RequestsFilter.allCases) { option in
                    Text(option.label).tag(option)
                }
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .frame(width: 280)
            .accessibilityIdentifier("requests-filter")
        }
    }

    @ViewBuilder
    private var status: some View {
        VStack(alignment: .leading, spacing: 8) {
            if store.isStale {
                let detail = store.errorMessage ?? "Refresh failed."
                requestsBanner(
                    "Showing the last available request history. \(detail)",
                    symbol: "wifi.exclamationmark",
                    color: .orange)
                    .accessibilityIdentifier("requests-stale")
            }
            if let snapshot = store.snapshot,
               let message = requestCoverageMessage(snapshot.coverage) {
                requestsBanner(
                    message,
                    symbol: "clock.badge.exclamationmark",
                    color: .orange)
                    .accessibilityIdentifier("requests-partial-history")
            }
        }
        .padding(.bottom, store.isStale || store.snapshot != nil ? 12 : 0)
    }

    @ViewBuilder
    private var content: some View {
        if store.isLoading, store.snapshot == nil {
            VStack(spacing: 12) {
                ProgressView()
                Text("Loading requests…")
                    .foregroundStyle(.secondary)
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
        } else if store.snapshot != nil {
            if items.isEmpty {
                emptyState
            } else {
                requestList
            }
        } else {
            errorState
        }
    }

    private var requestList: some View {
        ScrollView([.horizontal, .vertical]) {
            LazyVStack(alignment: .leading, spacing: 0, pinnedViews: [.sectionHeaders]) {
                Section {
                    ForEach(Array(items.enumerated()), id: \.element.eventID) {
                        index, item in
                        RequestTableRow(item: item, shaded: index.isMultiple(of: 2))
                    }

                    if store.canLoadOlder {
                        Button {
                            store.loadOlder()
                        } label: {
                            if store.isLoadingOlder {
                                HStack(spacing: 8) {
                                    ProgressView()
                                        .controlSize(.small)
                                    Text("Loading older requests…")
                                }
                            } else {
                                Text("Load Older")
                            }
                        }
                        .disabled(
                            store.isLoadingOlder
                                || store.isLoading
                                || store.isRefreshing)
                        .frame(width: RequestTableMetrics.totalWidth)
                        .frame(minHeight: 44)
                        .accessibilityIdentifier("requests-load-older")
                    }
                } header: {
                    RequestTableHeader()
                }
            }
            .padding(.horizontal, 28)
            .padding(.bottom, 28)
        }
        .accessibilityIdentifier("requests-table")
    }

    private var emptyState: some View {
        VStack(spacing: 12) {
            Image(systemName: "list.bullet.rectangle")
                .font(.system(size: 36))
                .foregroundStyle(.secondary)
            Text(requestsEmptyTitle(filter: store.filter))
                .font(.title3.weight(.semibold))
            Text(requestsEmptyDetail(filter: store.filter))
                .multilineTextAlignment(.center)
                .foregroundStyle(.secondary)
                .frame(maxWidth: 420)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    private var errorState: some View {
        VStack(spacing: 12) {
            Image(systemName: "exclamationmark.triangle")
                .font(.system(size: 34))
                .foregroundStyle(.orange)
            Text("Requests are unavailable")
                .font(.title3.weight(.semibold))
            Text(store.errorMessage ?? "The gateway did not return request history.")
                .multilineTextAlignment(.center)
                .foregroundStyle(.secondary)
            Button("Try Again") {
                store.refresh()
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

private enum RequestTableMetrics {
    static let time: CGFloat = 128
    static let harness: CGFloat = 132
    static let type: CGFloat = 100
    static let requested: CGFloat = 190
    static let routing: CGFloat = 330
    static let result: CGFloat = 120
    static let duration: CGFloat = 86
    static let fallback: CGFloat = 270
    static let totalWidth = time + harness + type + requested + routing
        + result + duration + fallback
}

private struct RequestTableHeader: View {
    var body: some View {
        HStack(spacing: 0) {
            headerCell("Time", width: RequestTableMetrics.time)
            headerCell("Harness", width: RequestTableMetrics.harness)
            headerCell("Type", width: RequestTableMetrics.type)
            headerCell("Requested", width: RequestTableMetrics.requested)
            headerCell("Routing", width: RequestTableMetrics.routing)
            headerCell("Result", width: RequestTableMetrics.result)
            headerCell("Duration", width: RequestTableMetrics.duration)
            headerCell("Fallback", width: RequestTableMetrics.fallback)
        }
        .frame(width: RequestTableMetrics.totalWidth)
        .background(.regularMaterial)
        .overlay(alignment: .bottom) {
            Divider()
        }
        .accessibilityAddTraits(.isHeader)
    }

    private func headerCell(_ title: String, width: CGFloat) -> some View {
        Text(title)
            .font(.caption.weight(.semibold))
            .foregroundStyle(.secondary)
            .padding(.horizontal, 10)
            .padding(.vertical, 9)
            .frame(width: width, alignment: .leading)
    }
}

private struct RequestTableRow: View {
    let item: RequestItem
    let shaded: Bool

    private var fallbackReason: String? {
        requestFallbackReason(item.fallback)
    }

    var body: some View {
        HStack(spacing: 0) {
            Text(requestCompletedAtLabel(item.completedAt))
                .font(.caption)
                .foregroundStyle(.secondary)
                .monospacedDigit()
                .requestTableCell(width: RequestTableMetrics.time)

            HStack(spacing: 7) {
                ClientBrandMark(clientName: item.client, size: 15)
                Text(clientDisplayName(item.client))
                    .lineLimit(1)
                if item.subagent == true {
                    Image(systemName: "person.2.fill")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .help("Subagent request")
                }
            }
            .requestTableCell(width: RequestTableMetrics.harness)

            RequestTypeCell(item: item)
                .requestTableCell(width: RequestTableMetrics.type)

            Text(item.requestedModel.isEmpty ? "Unknown model" : item.requestedModel)
                .lineLimit(1)
                .truncationMode(.middle)
                .help(item.requestedModel)
                .requestTableCell(width: RequestTableMetrics.requested)

            RequestRoutingCell(item: item)
                .requestTableCell(width: RequestTableMetrics.routing)

            RequestResultBadge(item: item)
                .requestTableCell(width: RequestTableMetrics.result)

            Text(item.durationMs.map(requestDurationLabel) ?? "Unknown")
                .font(.caption)
                .foregroundStyle(.secondary)
                .monospacedDigit()
                .requestTableCell(width: RequestTableMetrics.duration)

            Group {
                if let fallbackReason {
                    Label(
                        fallbackReason,
                        systemImage: "arrow.uturn.forward.circle.fill")
                        .font(.caption.weight(.medium))
                        .foregroundStyle(.orange)
                        .lineLimit(1)
                        .padding(.horizontal, 7)
                        .padding(.vertical, 4)
                        .background(
                            Color.orange.opacity(0.12),
                            in: Capsule())
                        .help(fallbackReason)
                        .accessibilityIdentifier("request-fallback-reason")
                } else {
                    Text("None")
                        .font(.caption)
                        .foregroundStyle(.tertiary)
                }
            }
            .requestTableCell(width: RequestTableMetrics.fallback)
        }
        .font(.callout)
        .frame(width: RequestTableMetrics.totalWidth)
        .frame(minHeight: 38)
        .background(shaded ? Color.secondary.opacity(0.04) : Color.clear)
        .overlay(alignment: .bottom) {
            Divider()
        }
    }
}

private struct RequestTypeCell: View {
    let item: RequestItem

    var body: some View {
        if let label = requestTypeLabel(item),
           let description = requestTypeDescription(item) {
            Text(label)
                .font(.caption.weight(.medium))
                .foregroundStyle(.secondary)
                .lineLimit(1)
                .help(description)
                .accessibilityElement(children: .ignore)
                .accessibilityLabel(description)
                .accessibilityIdentifier("request-type-auto-check")
        } else {
            Text("")
                .accessibilityHidden(true)
        }
    }
}

private struct RequestRoutingCell: View {
    let item: RequestItem

    var body: some View {
        Group {
            if let primary = item.primary {
                VStack(alignment: .leading, spacing: 2) {
                    Text(requestPrimaryRoutingLabel(primary))
                        .font(.caption)
                        .foregroundStyle(
                            primary.attempted ? Color.orange : Color.secondary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                    Text("→ \(requestServedProviderLabel(item))")
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
            } else {
                Text(requestServedProviderLabel(item))
                    .lineLimit(1)
                    .truncationMode(.middle)
            }
        }
        .help(requestRoutingAccessibilityLabel(item))
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(requestRoutingAccessibilityLabel(item))
    }
}

private struct RequestResultBadge: View {
    let item: RequestItem

    var body: some View {
        Text(requestResultLabel(item))
            .font(.caption.weight(.medium))
            .foregroundStyle(requestIsError(item) ? Color.red : Color.secondary)
            .lineLimit(1)
            .padding(.horizontal, 7)
            .padding(.vertical, 3)
            .background(
                (requestIsError(item) ? Color.red : Color.secondary)
                    .opacity(0.1),
                in: Capsule())
            .help(requestTerminationLabel(item.terminationReason) ?? "")
    }
}

private extension View {
    func requestTableCell(width: CGFloat) -> some View {
        padding(.horizontal, 10)
            .padding(.vertical, 7)
            .frame(width: width, alignment: .leading)
            .contentShape(Rectangle())
    }
}

func requestTypeLabel(_ item: RequestItem) -> String? {
    item.requestClassification?.isClaudeAutoPermissionCheck == true
        ? "Auto check"
        : nil
}

func requestTypeDescription(_ item: RequestItem) -> String? {
    guard requestTypeLabel(item) != nil else { return nil }
    if item.status == 401 {
        return "Switch identified this as a Claude Code Auto permission check "
            + "and routed it to Anthropic. Anthropic rejected the request "
            + "credentials. Auto checks require a Claude login or Anthropic "
            + "API key accepted by Anthropic."
    }
    if item.status == 403 {
        return "Switch identified this as a Claude Code Auto permission check "
            + "and routed it to Anthropic. Anthropic denied access to the "
            + "request. Check the Claude login or Anthropic API key, plus the "
            + "account and model permissions."
    }
    return "Switch identified this as a Claude Code Auto permission check "
        + "and routed it to Anthropic."
}

func requestServedProviderLabel(_ item: RequestItem) -> String {
    let model = item.servedModel
    let provider = item.effectiveProvider.capitalized
    switch (model.isEmpty, provider.isEmpty) {
    case (false, false):
        return "\(model) · \(provider)"
    case (false, true):
        return model
    case (true, false):
        return provider
    case (true, true):
        return "Unknown"
    }
}

func requestPrimaryProviderLabel(_ primary: RequestPrimaryAttempt) -> String {
    let model = primary.model
    let provider = primary.provider.capitalized
    switch (model.isEmpty, provider.isEmpty) {
    case (false, false):
        return "\(model) · \(provider)"
    case (false, true):
        return model
    case (true, false):
        return provider
    case (true, true):
        return "Unknown primary"
    }
}

func requestPrimaryStateLabel(_ primary: RequestPrimaryAttempt) -> String {
    guard primary.attempted else { return "Not attempted" }
    if primary.outcome == "http_error" {
        return primary.status.map { "HTTP \($0)" } ?? "HTTP error"
    }
    let label = requestFallbackLabel(primary.outcome)
    return label == "Fallback" ? "Attempted" : label
}

func requestPrimaryRoutingLabel(_ primary: RequestPrimaryAttempt) -> String {
    let route = requestPrimaryProviderLabel(primary)
    return primary.attempted ? route : "\(route) · Not attempted"
}

func requestRoutingAccessibilityLabel(_ item: RequestItem) -> String {
    let served = requestServedProviderLabel(item)
    guard let primary = item.primary else { return served }
    return "Primary \(requestPrimaryProviderLabel(primary)), "
        + "\(requestPrimaryStateLabel(primary)). Served by \(served)"
}

func requestResultLabel(_ item: RequestItem) -> String {
    if let status = item.status {
        return "HTTP \(status)"
    }
    if item.terminationReason == "completed" {
        return "Completed"
    }
    if let label = requestTerminationLabel(item.terminationReason) {
        return label
    }
    return "No status"
}

func requestIsFallback(_ item: RequestItem) -> Bool {
    item.isFallback
}

func requestIsError(_ item: RequestItem) -> Bool {
    item.isError
}

func requestFallbackReason(_ fallback: RequestFallback?) -> String? {
    guard fallback?.attempted == true else { return nil }
    return requestFallbackLabel(fallback?.trigger)
}

func requestModelLabel(_ item: RequestItem) -> String {
    let requested = item.requestedModel
    let served = item.servedModel
    if requested.isEmpty {
        return served.isEmpty ? "Unknown model" : served
    }
    if served.isEmpty || requested == served {
        return requested
    }
    return "\(requested) → \(served)"
}

func requestRouteLabel(_ item: RequestItem) -> String {
    let configured = item.configuredRoute
    let effective = item.effectiveProvider
    if configured.isEmpty {
        return effective.isEmpty ? "Unknown route" : effective.capitalized
    }
    if effective.isEmpty
        || configured.caseInsensitiveCompare(effective) == .orderedSame {
        return configured.capitalized
    }
    return "\(configured.capitalized) → \(effective.capitalized)"
}

func requestTimestamp<T>(_ value: T) -> Double {
    if let date = value as? Date {
        return date.timeIntervalSince1970
    }
    if let number = value as? NSNumber {
        return number.doubleValue
    }
    if let string = value as? String {
        if let numeric = Double(string) {
            return numeric
        }
        return ISO8601DateFormatter().date(from: string)?.timeIntervalSince1970
            ?? 0
    }
    return 0
}

func requestCompletedAtLabel<T>(_ value: T) -> String {
    let timestamp = requestTimestamp(value)
    guard timestamp > 0 else { return "Unknown time" }
    return Date(timeIntervalSince1970: timestamp).formatted(
        .dateTime
            .month(.twoDigits)
            .day(.twoDigits)
            .hour()
            .minute()
            .second())
}

func requestDurationLabel(_ milliseconds: Int64) -> String {
    if milliseconds < 1_000 {
        return "\(milliseconds) ms"
    }
    return (Double(milliseconds) / 1_000).formatted(
        .number.precision(.fractionLength(1))) + " s"
}

func requestTerminationLabel(_ reason: String?) -> String? {
    guard let reason, !reason.isEmpty, reason != "completed" else {
        return nil
    }
    return reason.replacingOccurrences(of: "_", with: " ").capitalized
}

func requestsEmptyTitle(filter: RequestsFilter) -> String {
    switch filter {
    case .all: "No requests recorded"
    case .fallbacks: "No fallback requests"
    case .errors: "No request errors"
    }
}

func requestsEmptyDetail(filter: RequestsFilter) -> String {
    switch filter {
    case .all:
        "Completed requests will appear here as the gateway records them."
    case .fallbacks:
        "Requests that use a fallback provider will appear here."
    case .errors:
        "Requests that end in an error will appear here."
    }
}

private func requestOutcomeLabel(_ item: RequestItem) -> some View {
    let isError = requestIsError(item)
    let label: String
    if let status = item.status {
        label = "HTTP \(status)"
    } else if item.terminationReason == "completed" {
        label = "Completed"
    } else {
        label = "No upstream status"
    }
    return Label(
        label,
        systemImage: isError
            ? "exclamationmark.circle.fill"
            : "checkmark.circle.fill")
        .foregroundStyle(isError ? Color.red : Color.secondary)
}

func requestCoverageMessage(_ coverage: RequestCoverage) -> String? {
    guard !coverage.complete else {
        return nil
    }
    return coverage.reason.isEmpty
        ? "Partial request history is available."
        : "Partial request history: \(coverage.reason)"
}

private func requestsBanner(
    _ text: String,
    symbol: String,
    color: Color
) -> some View {
    HStack(alignment: .top, spacing: 9) {
        Image(systemName: symbol)
            .foregroundStyle(color)
        Text(text)
            .font(.callout)
            .fixedSize(horizontal: false, vertical: true)
        Spacer()
    }
    .padding(11)
    .background(color.opacity(0.1), in: RoundedRectangle(cornerRadius: 9))
}
