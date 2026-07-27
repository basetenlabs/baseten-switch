import AppKit
import SwiftUI

enum ClientBrandMarkKind: Equatable {
    case claude
    case openAI
    case systemSymbol(String)
}

/// Brand identity for supported harnesses. The marks are visual labels only;
/// client identifiers from the gateway remain the source of routing behavior.
func clientBrandMarkKind(_ clientName: String) -> ClientBrandMarkKind {
    switch clientName {
    case "claude-code":
        return .claude
    case "codex":
        return .openAI
    default:
        return .systemSymbol(clientIconName(clientName))
    }
}

enum ClientBrandAssets {
    static let openAIBlossom: NSImage? = {
        guard let url = openAIBlossomResourceURL(),
              let image = NSImage(contentsOf: url) else {
            return nil
        }
        image.isTemplate = true
        return image
    }()

    static func openAIBlossomResourceURL() -> URL? {
        if let bundled = Bundle.main.url(
            forResource: "openai-blossom",
            withExtension: "svg")
        {
            return bundled
        }

        let packageRoot = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
        let sourceAsset = packageRoot
            .appendingPathComponent("Assets/openai-blossom.svg")
        return FileManager.default.fileExists(atPath: sourceAsset.path)
            ? sourceAsset
            : nil
    }

    /// AppKit projection of the same client identities used by SwiftUI.
    /// NSMenuItem cannot host a SwiftUI view, so render the Claude vector
    /// into a template image and copy the bundled OpenAI vector before sizing
    /// it for the menu. Unknown clients retain their existing SF Symbol.
    static func menuImage(
        for clientName: String,
        size: NSSize = NSSize(width: 16, height: 16)
    ) -> NSImage? {
        let image: NSImage?
        switch clientBrandMarkKind(clientName) {
        case .claude:
            image = NSImage(size: size, flipped: true) { rect in
                guard let context = NSGraphicsContext.current?.cgContext else {
                    return false
                }
                context.addPath(ClaudeBrandShape().path(in: rect).cgPath)
                context.setFillColor(NSColor.black.cgColor)
                context.fillPath()
                return true
            }
        case .openAI:
            image = openAIBlossom?.copy() as? NSImage
        case let .systemSymbol(symbol):
            image = NSImage(
                systemSymbolName: symbol,
                accessibilityDescription: nil)
        }

        image?.isTemplate = true
        image?.size = size
        return image
    }
}

struct ClientBrandMark: View {
    @Environment(\.colorScheme) private var colorScheme

    let clientName: String
    var size: CGFloat = 16

    private var solidMonochromeColor: Color {
        colorScheme == .dark ? .white : .black
    }

    @ViewBuilder
    var body: some View {
        switch clientBrandMarkKind(clientName) {
        case .claude:
            ClaudeBrandShape()
                .fill(solidMonochromeColor)
                .frame(width: size, height: size)
                .accessibilityHidden(true)
        case .openAI:
            if let image = ClientBrandAssets.openAIBlossom {
                Image(nsImage: image)
                    .resizable()
                    .renderingMode(.template)
                    .scaledToFit()
                    .foregroundStyle(Color.primary)
                    .frame(width: size, height: size)
                    .accessibilityHidden(true)
            } else {
                fallbackSymbol("chevron.left.forwardslash.chevron.right")
            }
        case let .systemSymbol(symbol):
            fallbackSymbol(symbol)
        }
    }

    private func fallbackSymbol(_ symbol: String) -> some View {
        Image(systemName: symbol)
            .resizable()
            .scaledToFit()
            .foregroundStyle(Color.primary)
            .frame(width: size, height: size)
            .accessibilityHidden(true)
    }
}
