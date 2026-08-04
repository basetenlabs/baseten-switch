import XCTest
@testable import BasetenSwitch

// Display-only formatting tests. The RouteLogic tests died with
// RouteLogic.swift: route math now lives exclusively in the gateway
// and is exercised by the Go test suite; the app's flips shell out to
// `baseten-switch on|off`.

final class DisplayTests: XCTestCase {
#if DEBUG
    func testAXHierarchyFixtureLevelParsing() {
        XCTAssertEqual(
            AXHierarchyFixtureLevel(rawValue: "selection"),
            .selection)
        XCTAssertNil(AXHierarchyFixtureLevel(rawValue: "unknown"))
    }
#endif

    @MainActor
    func testRegularApplicationHasConventionalMainMenu() {
        let hierarchy = makeApplicationMainMenu(
            displayName: "Baseten Switch Preview")

        XCTAssertEqual(
            hierarchy.menu.items.map(\.title),
            ["Baseten Switch Preview", "File", "Edit", "Window"])
        XCTAssertEqual(
            hierarchy.menu.items.first?.submenu?.items.first?.title,
            "About Baseten Switch Preview")
        XCTAssertTrue(
            hierarchy.menu.items.first?.submenu?.items.contains {
                $0.title == "Quit Baseten Switch Preview"
                    && $0.keyEquivalent == "q"
            } ?? false)
        XCTAssertTrue(
            hierarchy.windowsMenu.items.contains {
                $0.title == "Minimize" && $0.keyEquivalent == "m"
            })
        XCTAssertEqual(hierarchy.servicesMenu.title, "Services")
    }

    @MainActor
    func testStatusHeaderToggleModelWaitsForServerProjection() {
        var requested: [Bool] = []
        let model = StatusHeaderToggleModel(
            isOn: false,
            isEnabled: true,
            accessibilityStatus: "Off",
            accessibilityHelp: "Native providers only.",
            onToggle: {
                requested.append($0)
                return true
            })

        model.update(
            isOn: true,
            isEnabled: false,
            accessibilityStatus: "Applying",
            accessibilityHelp: "Wait for confirmation.")

        XCTAssertTrue(model.isOn)
        XCTAssertFalse(model.isEnabled)
        XCTAssertEqual(model.accessibilityStatus, "Applying")
        XCTAssertEqual(model.accessibilityHelp, "Wait for confirmation.")
        XCTAssertTrue(requested.isEmpty)

        XCTAssertFalse(model.requestChange(to: false))
        XCTAssertTrue(model.isOn)
        XCTAssertTrue(requested.isEmpty)

        model.update(
            isOn: false,
            isEnabled: true,
            accessibilityStatus: "Off",
            accessibilityHelp: "Native providers only.")
        XCTAssertTrue(model.requestChange(to: true))

        XCTAssertFalse(model.isOn)
        XCTAssertEqual(requested, [true])
        XCTAssertFalse(model.requestChange(to: true))
        XCTAssertEqual(requested, [true])

        model.update(
            isOn: true,
            isEnabled: false,
            accessibilityStatus: "Applying",
            accessibilityHelp: "Wait for confirmation.")
        XCTAssertTrue(model.isOn)
    }

    @MainActor
    func testStatusHeaderToggleModelRejectedRequestDoesNotDriftOrLatch() {
        var requested: [Bool] = []
        let model = StatusHeaderToggleModel(
            isOn: false,
            isEnabled: true,
            accessibilityStatus: "Off",
            accessibilityHelp: "Fixture state.",
            onToggle: {
                requested.append($0)
                return false
            })

        XCTAssertFalse(model.requestChange(to: true))
        XCTAssertFalse(model.isOn)
        XCTAssertFalse(model.requestChange(to: true))
        XCTAssertFalse(model.isOn)
        XCTAssertEqual(requested, [true, true])
    }

    func testStatusHeaderKeyboardCommandUsesNativeActivationKeys() {
        XCTAssertEqual(
            StatusHeaderKeyboardCommand.resolve(
                charactersIgnoringModifiers: " ",
                modifierFlags: [],
                isRepeat: false),
            .toggle)
        XCTAssertEqual(
            StatusHeaderKeyboardCommand.resolve(
                charactersIgnoringModifiers: "\r",
                modifierFlags: [],
                isRepeat: false),
            .toggle)
        XCTAssertEqual(
            StatusHeaderKeyboardCommand.resolve(
                charactersIgnoringModifiers: "\u{3}",
                modifierFlags: [.numericPad],
                isRepeat: false),
            .toggle)
        XCTAssertNil(
            StatusHeaderKeyboardCommand.resolve(
                charactersIgnoringModifiers: " ",
                modifierFlags: [],
                isRepeat: true))
        XCTAssertNil(
            StatusHeaderKeyboardCommand.resolve(
                charactersIgnoringModifiers: " ",
                modifierFlags: [.command],
                isRepeat: false))
        XCTAssertNil(
            StatusHeaderKeyboardCommand.resolve(
                charactersIgnoringModifiers: "\r",
                modifierFlags: [.shift],
                isRepeat: false))
        XCTAssertNil(
            StatusHeaderKeyboardCommand.resolve(
                charactersIgnoringModifiers: "x",
                modifierFlags: [],
                isRepeat: false))
    }

    @MainActor
    func testStatusMenuHeaderOffersFocusableSpaceActivation() throws {
        var requested: [Bool] = []
        let header = StatusMenuHeaderView(
            subtitle: "Native providers only",
            isOn: false,
            accessibilityStatus: "Off",
            switchEnabled: true,
            disabledReason: nil,
            onToggle: {
                requested.append($0)
                return true
            })
        let focusable = try XCTUnwrap(
            header.subviews.first(where: \.acceptsFirstResponder))
        XCTAssertTrue(focusable.canBecomeKeyView)

        let space = try XCTUnwrap(NSEvent.keyEvent(
            with: .keyDown,
            location: .zero,
            modifierFlags: [],
            timestamp: 0,
            windowNumber: 0,
            context: nil,
            characters: " ",
            charactersIgnoringModifiers: " ",
            isARepeat: false,
            keyCode: 49))
        focusable.keyDown(with: space)
        XCTAssertEqual(requested, [true])

        // The accepted request stays backend-authoritative and is not
        // dispatched again before a fresh state projection arrives.
        focusable.keyDown(with: space)
        XCTAssertEqual(requested, [true])

        header.update(
            subtitle: "Applying routing change…",
            isOn: true,
            accessibilityStatus: "Applying",
            switchEnabled: false,
            disabledReason: "Wait for confirmation.")
        XCTAssertFalse(focusable.acceptsFirstResponder)
        XCTAssertFalse(focusable.canBecomeKeyView)
    }

    func testMenubarIconUsesBasetenSVGAsTemplate() {
        let resourceURL = BasetenSwitchApp.menubarIconResourceURL()

        XCTAssertEqual(resourceURL?.lastPathComponent, "baseten-logo-white.svg")
        XCTAssertTrue(BasetenSwitchApp.menubarIcon.isTemplate)
        XCTAssertEqual(BasetenSwitchApp.menubarIcon.size,
                       NSSize(width: 18, height: 18))
        XCTAssertNotNil(BasetenSwitchApp.menubarIcon.tiffRepresentation)
    }

    func testClientBrandMarksUseOfficialMonochromeIdentities() throws {
        XCTAssertEqual(clientBrandMarkKind("claude-code"), .claude)
        XCTAssertEqual(clientBrandMarkKind("codex"), .openAI)
        XCTAssertEqual(
            clientBrandMarkKind("opencode"),
            .systemSymbol("curlybraces"))

        let resourceURL = ClientBrandAssets.openAIBlossomResourceURL()
        XCTAssertEqual(
            resourceURL?.lastPathComponent,
            "openai-blossom.svg")
        XCTAssertTrue(try XCTUnwrap(ClientBrandAssets.openAIBlossom).isTemplate)

        for clientName in ["claude-code", "codex", "opencode"] {
            let image = try XCTUnwrap(
                ClientBrandAssets.menuImage(for: clientName))
            XCTAssertTrue(image.isTemplate)
            XCTAssertEqual(image.size, NSSize(width: 16, height: 16))
            XCTAssertNotNil(
                image.tiffRepresentation,
                "\(clientName) should render into an AppKit menu image")
        }
    }

    func testActiveRoutingColorIsBasetenGreen() {
        guard let color = AppColors.basetenGreen.usingColorSpace(.sRGB) else {
            return XCTFail("Baseten green must be representable in sRGB")
        }

        XCTAssertEqual(color.redComponent, 0x16 / 255.0, accuracy: 0.0001)
        XCTAssertEqual(color.greenComponent, 0xD7 / 255.0, accuracy: 0.0001)
        XCTAssertEqual(color.blueComponent, 0x66 / 255.0, accuracy: 0.0001)
        XCTAssertEqual(color.alphaComponent, 1.0, accuracy: 0.0001)
    }

    func testStatusHeaderSwitchUsesGreenOnlyForOnTrack() throws {
        let on = try XCTUnwrap(
            StatusHeaderToggleAppearance.trackColor(isOn: true)
                .usingColorSpace(.sRGB))
        let off = try XCTUnwrap(
            StatusHeaderToggleAppearance.trackColor(isOn: false)
                .usingColorSpace(.sRGB))

        XCTAssertEqual(on.redComponent, 0x16 / 255.0, accuracy: 0.0001)
        XCTAssertEqual(on.greenComponent, 0xD7 / 255.0, accuracy: 0.0001)
        XCTAssertEqual(on.blueComponent, 0x66 / 255.0, accuracy: 0.0001)
        XCTAssertNotEqual(on, off)

        let offThumb = StatusHeaderToggleAppearance.thumbRect(isOn: false)
        let onThumb = StatusHeaderToggleAppearance.thumbRect(isOn: true)
        XCTAssertEqual(offThumb.minX,
                       StatusHeaderToggleAppearance.trackRect.minX
                           + StatusHeaderToggleAppearance.thumbInset)
        XCTAssertEqual(onThumb.maxX,
                       StatusHeaderToggleAppearance.trackRect.maxX
                           - StatusHeaderToggleAppearance.thumbInset)
        XCTAssertGreaterThan(onThumb.minX, offThumb.minX)
        XCTAssertEqual(
            StatusHeaderToggleAppearance.thumbShadowOpacity(isOn: false),
            0)
        XCTAssertEqual(
            StatusHeaderToggleAppearance.thumbShadowOpacity(isOn: true),
            StatusHeaderToggleAppearance.onThumbShadowOpacity)
    }

    func testStatusHeaderSwitchAnimationPolicy() {
        XCTAssertNil(StatusHeaderToggleAnimationPolicy.duration(
            previousState: nil,
            nextState: true,
            reduceMotion: false))
        XCTAssertNil(StatusHeaderToggleAnimationPolicy.duration(
            previousState: false,
            nextState: false,
            reduceMotion: false))
        XCTAssertNil(StatusHeaderToggleAnimationPolicy.duration(
            previousState: false,
            nextState: true,
            reduceMotion: true))
        XCTAssertEqual(
            StatusHeaderToggleAnimationPolicy.duration(
                previousState: false,
                nextState: true,
                reduceMotion: false),
            StatusHeaderToggleAnimationPolicy.stateChangeDuration)
    }

    @MainActor
    func testStatusHeaderSwitchSynchronizesStateAndAccessibility() {
        let model = StatusHeaderToggleModel(
            isOn: false,
            isEnabled: true,
            accessibilityStatus: "Off",
            accessibilityHelp: "Native providers only.",
            onToggle: { _ in true })
        let control = StatusHeaderToggleControl(model: model)

        XCTAssertTrue(control.isEnabled)
        XCTAssertEqual(control.accessibilityRole(), .checkBox)
        XCTAssertEqual(control.accessibilityValue() as? NSNumber, false)

        model.update(
            isOn: true,
            isEnabled: false,
            accessibilityStatus: "Applying",
            accessibilityHelp: "Wait for confirmation.")
        control.synchronize()

        XCTAssertFalse(control.isEnabled)
        XCTAssertEqual(control.accessibilityValue() as? NSNumber, true)
        XCTAssertEqual(
            control.accessibilityHelp(),
            "Current status: Applying. Wait for confirmation.")
    }

    func testRouterWindowToolbarContainsRefreshOnly() {
        XCTAssertEqual(
            RouterWindowToolbarItems.defaultIdentifiers.map(\.rawValue),
            ["co.baseten.switch.refresh"])
    }

    @MainActor
    func testGlobalRoutingTogglePresentationForMenuSurface() {
        let state = BasetenSwitchState(preview: .healthy)

        let fixturePresentation = globalRoutingTogglePresentation(
            state: state,
            isPreviewFixture: true)
        XCTAssertTrue(fixturePresentation.isOn)
        XCTAssertTrue(fixturePresentation.isEnabled)
        XCTAssertEqual(
            fixturePresentation.accessibilityStatus,
            "On")

        let livePresentation = globalRoutingTogglePresentation(
            state: state,
            isPreviewFixture: false)
        XCTAssertTrue(livePresentation.isOn)
        XCTAssertFalse(livePresentation.isEnabled)
        XCTAssertEqual(
            livePresentation.accessibilityStatus,
            "Unavailable")
    }

    @MainActor
    func testStatusHeaderSwitchAnimatesOnlyStateEndpointChanges() throws {
        let model = StatusHeaderToggleModel(
            isOn: false,
            isEnabled: true,
            accessibilityStatus: "Off",
            accessibilityHelp: "Native providers only.",
            onToggle: { _ in true })
        let control = StatusHeaderToggleControl(model: model)

        XCTAssertFalse(control.hasStateTransitionAnimationForTesting)
        XCTAssertEqual(
            control.thumbPositionForTesting.x,
            StatusHeaderToggleAppearance.thumbRect(isOn: false).midX)
        XCTAssertEqual(control.thumbShadowOpacityForTesting, 0)

        // Enabled-state and pending-label refreshes do not animate.
        model.update(
            isOn: false,
            isEnabled: false,
            accessibilityStatus: "Applying",
            accessibilityHelp: "Wait for confirmation.")
        control.synchronize(reduceMotion: false)
        XCTAssertFalse(control.hasStateTransitionAnimationForTesting)

        // A newly projected route state animates to the exact endpoint.
        model.update(
            isOn: true,
            isEnabled: true,
            accessibilityStatus: "On",
            accessibilityHelp: "Routing rules are active.")
        control.synchronize(reduceMotion: false)
        XCTAssertTrue(control.hasStateTransitionAnimationForTesting)
        XCTAssertEqual(
            control.thumbPositionForTesting.x,
            StatusHeaderToggleAppearance.thumbRect(isOn: true).midX)
        XCTAssertEqual(
            control.thumbShadowOpacityForTesting,
            StatusHeaderToggleAppearance.onThumbShadowOpacity)
        let shadowEndpoints = try XCTUnwrap(
            control.thumbShadowAnimationEndpointsForTesting)
        XCTAssertEqual(shadowEndpoints.0, 0)
        XCTAssertEqual(
            shadowEndpoints.1,
            StatusHeaderToggleAppearance.onThumbShadowOpacity)

        let green = try XCTUnwrap(
            control.trackColorForTesting?.usingColorSpace(.sRGB))
        XCTAssertEqual(green.redComponent, 0x16 / 255.0, accuracy: 0.0001)
        XCTAssertEqual(green.greenComponent, 0xD7 / 255.0, accuracy: 0.0001)
        XCTAssertEqual(green.blueComponent, 0x66 / 255.0, accuracy: 0.0001)

        // Reduce Motion snaps a subsequent endpoint change and removes any
        // in-flight state transition.
        model.update(
            isOn: false,
            isEnabled: true,
            accessibilityStatus: "Off",
            accessibilityHelp: "Native providers only.")
        control.synchronize(reduceMotion: true)
        XCTAssertFalse(control.hasStateTransitionAnimationForTesting)
        XCTAssertEqual(
            control.thumbPositionForTesting.x,
            StatusHeaderToggleAppearance.thumbRect(isOn: false).midX)
        XCTAssertEqual(control.thumbShadowOpacityForTesting, 0)
    }

    @MainActor
    func testStatusHeaderSwitchRendersGreenTrackOnlyWhenOn() throws {
        func renderedTrackColor(isOn: Bool, sample: NSPoint) throws -> NSColor {
            let model = StatusHeaderToggleModel(
                isOn: isOn,
                isEnabled: true,
                accessibilityStatus: isOn ? "On" : "Off",
                accessibilityHelp: "Fixture state.",
                onToggle: { _ in true })
            let control = StatusHeaderToggleControl(model: model)
            control.appearance = NSAppearance(named: .aqua)
            control.frame = NSRect(
                origin: .zero,
                size: StatusHeaderToggleAppearance.controlSize)

            let width = Int(control.bounds.width)
            let height = Int(control.bounds.height)
            let bitmap = try XCTUnwrap(NSBitmapImageRep(
                bitmapDataPlanes: nil,
                pixelsWide: width,
                pixelsHigh: height,
                bitsPerSample: 8,
                samplesPerPixel: 4,
                hasAlpha: true,
                isPlanar: false,
                colorSpaceName: .deviceRGB,
                bytesPerRow: 0,
                bitsPerPixel: 0))
            let context = try XCTUnwrap(NSGraphicsContext(bitmapImageRep: bitmap))

            NSGraphicsContext.saveGraphicsState()
            NSGraphicsContext.current = context
            control.layer?.render(in: context.cgContext)
            context.flushGraphics()
            NSGraphicsContext.restoreGraphicsState()

            return try XCTUnwrap(
                bitmap.colorAt(x: Int(sample.x), y: Int(sample.y))?
                    .usingColorSpace(NSColorSpace.sRGB))
        }

        // Sample the side opposite the thumb, well inside the track's
        // antialiased edge and shadow. This stays stable at a logical 1× scale.
        let on = try renderedTrackColor(
            isOn: true,
            sample: NSPoint(
                x: StatusHeaderToggleAppearance.trackRect.minX + 8,
                y: StatusHeaderToggleAppearance.trackRect.midY))
        let off = try renderedTrackColor(
            isOn: false,
            sample: NSPoint(
                x: StatusHeaderToggleAppearance.trackRect.maxX - 8,
                y: StatusHeaderToggleAppearance.trackRect.midY))

        XCTAssertEqual(on.redComponent, 0x16 / 255.0, accuracy: 1.0 / 255.0)
        XCTAssertEqual(on.greenComponent, 0xD7 / 255.0, accuracy: 1.0 / 255.0)
        XCTAssertEqual(on.blueComponent, 0x66 / 255.0, accuracy: 1.0 / 255.0)
        XCTAssertGreaterThan(
            abs(off.redComponent - on.redComponent),
            0.1)
    }

    func testCLIEnvironmentTargetsRunningRouterConfig() {
        XCTAssertEqual(cliEnvironmentOverrides(configPath: ""), [:])
        XCTAssertEqual(
            cliEnvironmentOverrides(configPath: "/tmp/live/gateway.yaml"),
            ["BASETEN_SWITCH_CONFIG_PATH": "/tmp/live/gateway.yaml"])
    }

    func testRouterWindowPresentationTransitionsAreIdempotent() {
        var state = RouterWindowPresentationState()
        XCTAssertFalse(state.isOpen)
        XCTAssertTrue(state.transition(to: true))
        XCTAssertTrue(state.isOpen)
        XCTAssertFalse(state.transition(to: true))
        XCTAssertTrue(state.transition(to: false))
        XCTAssertFalse(state.isOpen)
        XCTAssertFalse(state.transition(to: false))
    }

    @MainActor
    func testRouterWindowContentUsesNonObscuredLayoutGuide() throws {
        let window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 820, height: 600),
            styleMask: [.titled, .fullSizeContentView],
            backing: .buffered,
            defer: false)
        let hosted = NSViewController()
        hosted.view = NSView()

        installRouterWindowContentController(hosted, in: window)

        let container = try XCTUnwrap(window.contentViewController)
        XCTAssertFalse(container === hosted)
        XCTAssertTrue(hosted.parent === container)
        XCTAssertTrue(hosted.view.superview === container.view)
        XCTAssertFalse(hosted.view.translatesAutoresizingMaskIntoConstraints)
        let guide = try XCTUnwrap(
            window.contentLayoutGuide as? NSLayoutGuide)
        var ancestor: NSView? = hosted.view
        var allConstraints: [NSLayoutConstraint] = []
        while let view = ancestor {
            allConstraints.append(contentsOf: view.constraints)
            ancestor = view.superview
        }
        let guideConstraints = allConstraints.filter { constraint in
            let firstMatches = constraint.firstItem === hosted.view
                && constraint.secondItem === guide
            let secondMatches = constraint.secondItem === hosted.view
                && constraint.firstItem === guide
            return constraint.isActive && (firstMatches || secondMatches)
        }
        XCTAssertEqual(guideConstraints.count, 4)
    }

    @MainActor
    func testRouterWindowShowPreservesSelectionUnlessClientIsExplicit() {
        let navigation = RouterWindowNavigation()

        navigation.prepareForShow(clientName: nil)
        XCTAssertEqual(navigation.selection, .overview)

        navigation.prepareForShow(clientName: "claude-code")
        XCTAssertEqual(navigation.selection, .client("claude-code"))

        navigation.prepareForShow(clientName: nil)
        XCTAssertEqual(navigation.selection, .client("claude-code"))
    }

    func testFormatUptime() {
        XCTAssertEqual(formatUptime(0), "0s")
        XCTAssertEqual(formatUptime(-5), "0s")
        XCTAssertEqual(formatUptime(59), "59s")
        XCTAssertEqual(formatUptime(60), "1m 0s")
        XCTAssertEqual(formatUptime(252), "4m 12s")
        XCTAssertEqual(formatUptime(3600), "1h 0m")
        XCTAssertEqual(formatUptime(3720), "1h 2m")
    }

    func testPresentationEqualityIgnoresMonotonicUptimePolls() {
        func snapshot(uptime: Int64,
                      observedAt: TimeInterval,
                      activeRequests: Int = 0)
            -> RoutingSnapshot {
            RoutingSnapshot(
                status: AdminStatusSnapshot(dict: [
                    "router_boot_id": "boot-a",
                    "active_generation": 7,
                    "active_config_hash": "sha256:same",
                    "desired_config_hash": "sha256:same",
                    "health": "ready",
                    "version": "v1",
                    "uptime_seconds": NSNumber(value: uptime),
                    "active_requests": NSNumber(value: activeRequests),
                                        "capabilities": ["global_routing"],
                    "global_routing_enabled": true,
                    "clients": [],
                ]),
                observedAt: Date(timeIntervalSince1970: observedAt))
        }

        XCTAssertTrue(routingPresentationEqual(
            snapshot(uptime: 100, observedAt: 1_000),
            snapshot(uptime: 105, observedAt: 1_005)))
        XCTAssertFalse(routingPresentationEqual(
            snapshot(uptime: 100, observedAt: 1_000),
            snapshot(uptime: 101, observedAt: 1_001, activeRequests: 1)))
        XCTAssertEqual(
            projectedUptimeSeconds(
                snapshot: snapshot(uptime: 100, observedAt: 1_000),
                now: Date(timeIntervalSince1970: 1_007.9)),
            107)
    }

    func testGatewayStatusLabel() {
        XCTAssertEqual(gatewayStatusLabel(up: true, uptimeSeconds: 3720),
                       "Gateway: up 1h 2m")
        XCTAssertEqual(gatewayStatusLabel(up: false, uptimeSeconds: 0),
                       "Gateway: not running")
        // uptime is ignored when down; never render a stale duration.
        XCTAssertEqual(gatewayStatusLabel(up: false, uptimeSeconds: 3720),
                       "Gateway: not running")
    }

    func testClientRowLabelOnBaseten() {
        XCTAssertEqual(
            clientRowLabel(name: "claude-code", route: "baseten",
                           nativeRoute: "anthropic", selectedModel: "zai-org/GLM-5.2"),
            "claude-code -> zai-org/GLM-5.2")
        // Baseten route with no resolved model: make the gap visible.
        XCTAssertEqual(
            clientRowLabel(name: "claude-code", route: "baseten",
                           nativeRoute: "anthropic", selectedModel: ""),
            "claude-code -> Baseten (?)")
    }

    func testClientRowLabelOnNative() {
        XCTAssertEqual(
            clientRowLabel(name: "codex", route: "openai",
                           nativeRoute: "openai", selectedModel: ""),
            "codex -> Native (openai)")
        // Empty route falls back to the status API's native_route.
        XCTAssertEqual(
            clientRowLabel(name: "claude-code", route: "",
                           nativeRoute: "anthropic", selectedModel: ""),
            "claude-code -> Native (anthropic)")
        // Nothing known: name only, never an empty "Native" suffix.
        XCTAssertEqual(
            clientRowLabel(name: "claude-code", route: "",
                           nativeRoute: "", selectedModel: ""),
            "claude-code")
    }

    func testMenuErrorLabel() {
        // Short messages pass through untouched.
        XCTAssertEqual(menuErrorLabel("baseten-switch on codex failed (exit 1)"),
                       "baseten-switch on codex failed (exit 1)")
        // Newlines collapse to a single menu-safe line.
        XCTAssertEqual(menuErrorLabel("line one\nline two"),
                       "line one line two")
        // Long messages truncate to the limit, ending in "...".
        let long = String(repeating: "x", count: 200)
        let out = menuErrorLabel(long)
        XCTAssertEqual(out.count, 80)
        XCTAssertTrue(out.hasSuffix("..."))
        XCTAssertTrue(out.hasPrefix("xxx"))
    }

    func testStableVariantDefaultsPreserveExistingIdentity() {
        let variant = AppVariant.resolve(
            infoDictionary: [:],
            homeDirectory: "/tmp/baseten-switch-home",
            environment: [:])

        XCTAssertEqual(variant.channel, .stable)
        XCTAssertEqual(variant.bundleIdentifier,
                       "co.baseten.switch")
        XCTAssertEqual(variant.displayName, "Baseten Switch")
        XCTAssertEqual(variant.executableName, "BasetenSwitch")
        XCTAssertEqual(variant.statusItemAutosaveName, "baseten-switch-toggle")
        XCTAssertEqual(variant.windowFrameAutosaveName,
                       "baseten-switch-router-window")
        XCTAssertTrue(variant.allowsLoginItem)
        XCTAssertEqual(variant.runtime.adminBaseURL.absoluteString,
                       "http://127.0.0.1:45273")
    }

    func testPreviewVariantUsesIndependentIdentityAndRuntime() {
        let variant = AppVariant.resolve(
            infoDictionary: [
                "BasetenSwitchBuildChannel": "preview",
                "CFBundleDisplayName": "Baseten Switch Preview",
                "CFBundleExecutable": "BasetenSwitchPreview",
            ],
            bundleIdentifier: "co.baseten.switch.preview",
            runningExecutableName: "BasetenSwitchPreview",
            homeDirectory: "/tmp/baseten-switch-home",
            environment: ["BASETEN_SWITCH_GATEWAY_BIN": "/workspace/bin/baseten-switch"])

        XCTAssertEqual(variant.channel, .preview)
        XCTAssertEqual(variant.bundleIdentifier,
                       "co.baseten.switch.preview")
        XCTAssertEqual(variant.displayName, "Baseten Switch Preview")
        XCTAssertEqual(variant.executableName, "BasetenSwitchPreview")
        XCTAssertNil(variant.identityError)
        XCTAssertEqual(variant.statusItemAutosaveName,
                       "baseten-switch-toggle-preview")
        XCTAssertEqual(variant.windowFrameAutosaveName,
                       "baseten-switch-router-window-preview")
        XCTAssertFalse(variant.allowsLoginItem)
        XCTAssertEqual(variant.runtime.adminBaseURL.absoluteString,
                       "http://127.0.0.1:45373")
        XCTAssertEqual(
            variant.runtime.expectedConfigPath,
            "/tmp/baseten-switch-home/.config/baseten-switch-preview/gateway.yaml")

        let environment = variant.runtime.environment
        XCTAssertEqual(environment["BASETEN_SWITCH_CONFIG_PATH"],
                       variant.runtime.expectedConfigPath)
        XCTAssertEqual(environment["BASETEN_SWITCH_GATEWAY_BIN"],
                       "/workspace/bin/baseten-switch")
        XCTAssertEqual(environment["BASETEN_SWITCH_GATEWAY_ADMIN"],
                       "http://127.0.0.1:45373")
        XCTAssertEqual(environment["BASETEN_SWITCH_GATEWAY_PORT"], "45373")
        XCTAssertEqual(environment["BASETEN_SWITCH_DOOR_PORTS"], "45371")
        XCTAssertEqual(environment["BASETEN_SWITCH_GATEWAY_PID"],
                       "/tmp/baseten-switch-home/.config/baseten-switch-preview/gateway.pid")
        XCTAssertEqual(environment["BASETEN_SWITCH_TELEMETRY_DIR"],
                       "/tmp/baseten-switch-home/.config/baseten-switch-preview/telemetry")
        XCTAssertNil(environment["BASETEN_SWITCH_TELEMETRY_LOG"])
        XCTAssertEqual(environment["BASETEN_SWITCH_LAUNCHD"], "off")
        XCTAssertEqual(environment["BASETEN_SWITCH_MENUBAR"], "off")
        XCTAssertEqual(environment["BASETEN_SWITCH_AUTH_NO_KEYRING"], "1")
        XCTAssertEqual(environment["BASETEN_SWITCH_PRIVATE_RUNTIME"], "1")
        XCTAssertEqual(environment["BASETEN_SWITCH_OAUTH_PROFILE"],
                       "baseten-switch-preview")
        XCTAssertEqual(environment["BASETEN_SWITCH_CLAUDE_SETTINGS"],
                       "/tmp/baseten-switch-home/.config/baseten-switch-preview/claude/settings.json")
        XCTAssertEqual(environment["BASETEN_CONFIG_DIR"],
                       "/tmp/baseten-switch-home/.config/baseten-switch-preview/baseten")
        XCTAssertEqual(environment["BASETEN_SWITCH_GATEWAY_TOKEN"],
                       "baseten-switch-local-gateway-preview")
        for key in [
            "BASETEN_API_KEY", "BASETEN_SWITCH_API_KEY", "BASETEN_SWITCH_API_KEY_FALLBACK",
            "ANTHROPIC_API_KEY", "BASETEN_SWITCH_ANTHROPIC_KEY", "ANTHROPIC_AUTH_TOKEN",
            "OPENAI_API_KEY", "CODEX_AUTH_TOKEN",
        ] {
            XCTAssertEqual(environment[key], "", "\(key) must be cleared")
        }
    }

    func testMalformedPreviewMetadataNeverFallsBackToStable() {
        let missingChannel = AppVariant.resolve(
            infoDictionary: [
                "CFBundleDisplayName": "Baseten Switch Preview",
                "CFBundleExecutable": "BasetenSwitchPreview",
            ],
            bundleIdentifier: "co.baseten.switch.preview",
            runningExecutableName: "BasetenSwitchPreview",
            homeDirectory: "/tmp/baseten-switch-home",
            environment: [:])

        XCTAssertEqual(missingChannel.channel, .preview)
        XCTAssertEqual(
            missingChannel.runtime.expectedConfigPath,
            "/tmp/baseten-switch-home/.config/baseten-switch-preview/gateway.yaml")
        XCTAssertNotNil(missingChannel.identityError)
        XCTAssertFalse(missingChannel.allowsLoginItem)

        let wrongExecutable = AppVariant.resolve(
            infoDictionary: [
                "BasetenSwitchBuildChannel": "preview",
                "CFBundleDisplayName": "Baseten Switch Preview",
                "CFBundleExecutable": "BasetenSwitchPreview",
            ],
            bundleIdentifier: "co.baseten.switch.preview",
            runningExecutableName: "BasetenSwitch",
            homeDirectory: "/tmp/baseten-switch-home",
            environment: [:])
        XCTAssertEqual(wrongExecutable.channel, .preview)
        XCTAssertTrue(
            wrongExecutable.identityError?.contains("running executable")
                == true)
        XCTAssertFalse(wrongExecutable.allowsLoginItem)
    }

    func testPreviewRuntimeTrustValidatesConfigAndPortTuple() {
        let variant = AppVariant.resolve(
            infoDictionary: [
                "BasetenSwitchBuildChannel": "preview",
                "CFBundleDisplayName": "Baseten Switch Preview",
                "CFBundleExecutable": "BasetenSwitchPreview",
            ],
            bundleIdentifier: "co.baseten.switch.preview",
            runningExecutableName: "BasetenSwitchPreview",
            homeDirectory: "/tmp/baseten-switch-home",
            environment: [:])
        let configPath =
            "/tmp/baseten-switch-home/.config/baseten-switch-preview/gateway.yaml"
        func snapshot(bindAddr: String) -> RoutingSnapshot {
            RoutingSnapshot(
                status: AdminStatusSnapshot(dict: [
                    "router_boot_id": "preview-boot",
                    "active_generation": 1,
                    "active_config_hash": "sha256:active",
                    "desired_config_hash": "sha256:active",
                                        "capabilities": ["global_routing"],
                    "config_path": configPath,
                    "global_routing_enabled": true,
                    "clients": [[
                        "name": "claude-code",
                        "enabled": true,
                        "bind_addr": bindAddr,
                    ]],
                ]),
                observedAt: Date())
        }

        XCTAssertEqual(
            runtimeTrustForSnapshot(
                variant: variant,
                snapshot: snapshot(bindAddr: "127.0.0.1:45372"),
                gatewayUp: true),
            .previewTrusted)
        guard case .identityMismatch(let reason) = runtimeTrustForSnapshot(
            variant: variant,
            snapshot: snapshot(bindAddr: "127.0.0.1:45272"),
            gatewayUp: true) else {
            return XCTFail("expected the Stable router port to fail closed")
        }
        XCTAssertTrue(reason.contains("127.0.0.1:45372"))
    }

    func testPreviewDoesNotInheritAmbientStableEndpoints() {
        let variant = AppVariant.resolve(
            infoDictionary: ["BasetenSwitchBuildChannel": "preview"],
            homeDirectory: "/tmp/baseten-switch-home",
            environment: [
                "BASETEN_SWITCH_ADMIN_URL": "http://127.0.0.1:45273",
            ])

        XCTAssertEqual(adminBaseURL(runtime: variant.runtime),
                       URL(string: "http://127.0.0.1:45373/")!)
        XCTAssertNil(variant.runtime.environment["BASETEN_SWITCH_GATEWAY_BIN"])
        XCTAssertNil(BasetenSwitchState.locateBasetenSwitchBinary(variant: variant))
    }

    func testPreviewCLIEnvironmentIsCompleteAndIgnoresReportedConfig() {
        let variant = AppVariant.resolve(
            infoDictionary: ["BasetenSwitchBuildChannel": "preview"],
            homeDirectory: "/tmp/baseten-switch-home",
            environment: ["BASETEN_SWITCH_GATEWAY_BIN": "/workspace/bin/baseten-switch"])
        let overrides = cliEnvironmentOverrides(
            configPath: "/tmp/stable/gateway.yaml", variant: variant)

        XCTAssertEqual(overrides, variant.runtime.environment)
        XCTAssertEqual(overrides["BASETEN_SWITCH_CONFIG_PATH"],
                       "/tmp/baseten-switch-home/.config/baseten-switch-preview/gateway.yaml")
        XCTAssertEqual(overrides["BASETEN_SWITCH_GATEWAY_PIDFILE"],
                       "/tmp/baseten-switch-home/.config/baseten-switch-preview/gateway.pid")
        XCTAssertEqual(overrides["BASETEN_SWITCH_DOOR_PIDFILE"],
                       "/tmp/baseten-switch-home/.config/baseten-switch-preview/door.pid")
        XCTAssertNil(overrides["BASETEN_SWITCH_TIER_FILE"])
        XCTAssertNil(overrides["BASETEN_SWITCH_TIERS_FILE"])
    }

    func testPreviewRuntimeTrustFailsClosed() {
        let expected = "/tmp/baseten-switch-home/.config/baseten-switch-preview/gateway.yaml"
        XCTAssertEqual(
            runtimeTrustForStatus(
                channel: .preview,
                expectedConfigPath: expected,
                reportedConfigPath: nil,
                gatewayUp: false),
            .previewDown)
        XCTAssertEqual(
            runtimeTrustForStatus(
                channel: .preview,
                expectedConfigPath: expected,
                reportedConfigPath: expected,
                gatewayUp: true),
            .previewTrusted)
        XCTAssertEqual(
            runtimeTrustForStatus(
                channel: .preview,
                expectedConfigPath: expected,
                reportedConfigPath: "/tmp/stable/gateway.yaml",
                gatewayUp: true),
            .previewMismatch(
                expected: expected,
                reported: "/tmp/stable/gateway.yaml"))
        XCTAssertEqual(
            runtimeTrustForStatus(
                channel: .stable,
                expectedConfigPath: nil,
                reportedConfigPath: "/tmp/anything",
                gatewayUp: false),
            .stable)
    }

    func testPreviewMenubarIconKeepsStableOpticalSizeWithBadge() {
        let preview = AppVariant.resolve(
            infoDictionary: ["BasetenSwitchBuildChannel": "preview"],
            homeDirectory: "/tmp/baseten-switch-home",
            environment: [:])
        let image = BasetenSwitchApp.menubarIcon(for: preview)

        XCTAssertEqual(image.size, NSSize(width: 18, height: 18))
        XCTAssertTrue(image.isTemplate)
    }

    // Binary lookup: whatever the chain resolves must be executable.
    // The candidate order ($BASETEN_SWITCH_GATEWAY_BIN, ~/.local/bin, brew opt
    // paths, no repo-dev path) is pinned by code review; env vars
    // cannot be varied per-test portably.
    func testLocateBinaryReturnsExecutableOrNil() {
        if let url = BasetenSwitchState.locateBasetenSwitchBinary() {
            XCTAssertTrue(FileManager.default.isExecutableFile(atPath: url.path))
        }
    }

    private func client(_ name: String, route: String, enabled: Bool = true,
                        fallback: Bool = false) -> ClientStatus {
        ClientStatus(dict: [
            "name": name, "enabled": enabled, "bind_addr": "127.0.0.1:18081",
            "protocol_shape": "anthropic", "effective_route": route, "native_route": "anthropic",
            "auth_set": true, "currently_bound": true,
            "fallback": ["active": fallback],
        ])!
    }

    private func clientWithSubagent(_ name: String, route: String,
                                   subagentModel: String,
                                   subagentRouting: String) -> ClientStatus {
        ClientStatus(dict: [
            "name": name, "enabled": true, "bind_addr": "127.0.0.1:18081",
            "protocol_shape": "anthropic", "effective_route": route, "native_route": "anthropic",
            "auth_set": true, "currently_bound": true,
            "fallback": ["active": false],
            "subagent_model": subagentModel, "subagent_routing": subagentRouting,
            "subagent_effective":
                (subagentRouting == "off" || subagentModel.isEmpty)
                ? "inherit"
                : subagentModel,
        ])!
    }

    // Decision table for the three-state icon: degraded > active > off.
    func testMenubarIconState() {
        // Active: gateway up, an enabled client routed to Baseten, no fallback.
        XCTAssertEqual(menubarIconState(gatewayUp: true,
                                        clients: [client("claude-code", route: "baseten")]),
                       .active)
        XCTAssertEqual(menubarIconState(gatewayUp: true,
                                        clients: [client("claude-code", route: "anthropic"),
                                                  client("codex", route: "baseten")]),
                       .active)
        // Degraded: any enabled client with an active fallback, on any route.
        XCTAssertEqual(menubarIconState(gatewayUp: true,
                                        clients: [client("claude-code", route: "baseten",
                                                         fallback: true)]),
                       .degraded)
        XCTAssertEqual(menubarIconState(gatewayUp: true,
                                        clients: [client("claude-code", route: "anthropic",
                                                         fallback: true)]),
                       .degraded)
        // Degraded outranks active across clients.
        XCTAssertEqual(menubarIconState(gatewayUp: true,
                                        clients: [client("claude-code", route: "baseten"),
                                                  client("codex", route: "openai",
                                                         fallback: true)]),
                       .degraded)
        // Disabled clients never influence the icon, for either state.
        XCTAssertEqual(menubarIconState(gatewayUp: true,
                                        clients: [client("claude-code", route: "baseten",
                                                         enabled: false, fallback: true)]),
                       .off)
        XCTAssertEqual(menubarIconState(gatewayUp: true,
                                        clients: [client("claude-code", route: "baseten"),
                                                  client("codex", route: "openai",
                                                         enabled: false, fallback: true)]),
                       .active)
        // Gateway down is off, even with stale routes or fallback flags.
        XCTAssertEqual(menubarIconState(gatewayUp: false,
                                        clients: [client("claude-code", route: "baseten",
                                                         fallback: true)]),
                       .off)
        // Off: no routed clients, or no clients at all.
        XCTAssertEqual(menubarIconState(gatewayUp: true,
                                        clients: [client("claude-code", route: "anthropic")]),
                       .off)
        XCTAssertEqual(menubarIconState(gatewayUp: true, clients: []), .off)
    }

    func testMenubarActivityTemporarilyUsesActiveColor() {
        XCTAssertEqual(menubarIconState(.off, hasActivity: true), .active)
        XCTAssertEqual(menubarIconState(.degraded, hasActivity: true), .active)
        XCTAssertEqual(menubarIconState(.degraded, hasActivity: false), .degraded)
    }

    private func authWithHealth(_ health: String) -> AuthStatus {
        AuthStatus(dict: ["signed_in": true, "health": health])
    }

    // A dead credential (health refresh_failed) turns the icon amber
    // regardless of route state, so the user notices without opening
    // the popup. Every other health leaves the decision table alone.
    func testMenubarIconStateAuthDead() {
        // Dead credential warns on any route state, including all-off.
        XCTAssertEqual(menubarIconState(gatewayUp: true, clients: [],
                                        auth: authWithHealth("refresh_failed")),
                       .degraded)
        XCTAssertEqual(menubarIconState(gatewayUp: true,
                                        clients: [client("claude-code", route: "anthropic")],
                                        auth: authWithHealth("refresh_failed")),
                       .degraded)
        // Outranks active.
        XCTAssertEqual(menubarIconState(gatewayUp: true,
                                        clients: [client("claude-code", route: "baseten")],
                                        auth: authWithHealth("refresh_failed")),
                       .degraded)
        // A down gateway stays off; the app has no live health then.
        XCTAssertEqual(menubarIconState(gatewayUp: false, clients: [],
                                        auth: authWithHealth("refresh_failed")),
                       .off)
        // Non-dead healths and a missing auth block change nothing.
        for health in ["ok", "error", "signed_out", ""] {
            XCTAssertEqual(menubarIconState(gatewayUp: true,
                                            clients: [client("claude-code", route: "baseten")],
                                            auth: authWithHealth(health)),
                           .active, "health \(health)")
        }
        XCTAssertEqual(menubarIconState(gatewayUp: true,
                                        clients: [client("claude-code", route: "baseten")],
                                        auth: nil),
                       .active)
    }

    // MARK: - Reauthenticate dispatch (Terminal via osascript)

    func testShellQuote() {
        XCTAssertEqual(shellQuote("/Users/x/.local/bin/baseten-switch"),
                       "'/Users/x/.local/bin/baseten-switch'")
        // Spaces stay inside the quotes; embedded single quotes use
        // the '\'' splice.
        XCTAssertEqual(shellQuote("/Users/x y/bin"), "'/Users/x y/bin'")
        XCTAssertEqual(shellQuote("a'b"), #"'a'\''b'"#)
    }

    func testAppleScriptStringLiteral() {
        XCTAssertEqual(appleScriptStringLiteral("plain"), "\"plain\"")
        XCTAssertEqual(appleScriptStringLiteral(#"say "hi""#),
                       #""say \"hi\"""#)
        // Backslashes escape before quotes so the two passes compose.
        XCTAssertEqual(appleScriptStringLiteral(#"a\b"#), #""a\\b""#)
    }

    // The Terminal command is the fixed "auth login" verb on the
    // resolved binary path; the script never carries server data
    // (command-injection surface stays closed).
    func testReauthAppleScript() {
        XCTAssertEqual(
            reauthAppleScript(binaryPath: "/Users/x/.local/bin/baseten-switch"),
            """
            tell application "Terminal"
                activate
                do script "'/Users/x/.local/bin/baseten-switch' auth login"
            end tell
            """)
        // A hostile path cannot break out of the quoting layers.
        let script = reauthAppleScript(binaryPath: #"/tmp/a"; rm -rf ~"#)
        XCTAssertTrue(script.contains(
            #"do script "'/tmp/a\"; rm -rf ~' auth login""#))
    }

    func testClientStatusParsesFallbackStatus() {
        XCTAssertTrue(client("claude-code", route: "baseten", fallback: true).fallbackActive)
        XCTAssertFalse(client("claude-code", route: "baseten").fallbackActive)
        let minimal = ClientStatus(dict: ["name": "codex"])!
        XCTAssertFalse(minimal.fallbackActive)
    }

    func testClientRowLabelFallbackSuffix() {
        // Baseten route with a resolved model.
        XCTAssertEqual(
            clientRowLabel(name: "claude-code", route: "baseten",
                           nativeRoute: "anthropic", selectedModel: "zai-org/GLM-5.2",
                           fallbackActive: true),
            "claude-code -> zai-org/GLM-5.2 (fallback active)")
        // Baseten route with no resolved model.
        XCTAssertEqual(
            clientRowLabel(name: "claude-code", route: "baseten",
                           nativeRoute: "anthropic", selectedModel: "",
                           fallbackActive: true),
            "claude-code -> Baseten (?) (fallback active)")
        // Native route.
        XCTAssertEqual(
            clientRowLabel(name: "codex", route: "openai",
                           nativeRoute: "openai", selectedModel: "",
                           fallbackActive: true),
            "codex -> Native (openai) (fallback active)")
        // Nothing known: suffix still renders after the bare name.
        XCTAssertEqual(
            clientRowLabel(name: "claude-code", route: "",
                           nativeRoute: "", selectedModel: "",
                           fallbackActive: true),
            "claude-code (fallback active)")
        // No fallback: label unchanged.
        XCTAssertEqual(
            clientRowLabel(name: "codex", route: "openai",
                           nativeRoute: "openai", selectedModel: "",
                           fallbackActive: false),
            "codex -> Native (openai)")
    }

    // Subagent fields decode from the admin status dict; absent keys
    // default to empty strings (unknown-field tolerance).
    func testClientStatusParsesSubagentFields() {
        let with = clientWithSubagent("claude-code", route: "baseten",
                                      subagentModel: "claude-baseten-glm-5-2",
                                      subagentRouting: "on")
        XCTAssertEqual(with.subagentModel, "claude-baseten-glm-5-2")
        XCTAssertEqual(with.subagentRouting, "on")

        let off = clientWithSubagent("claude-code", route: "baseten",
                                     subagentModel: "claude-baseten-glm-5-2",
                                     subagentRouting: "off")
        XCTAssertEqual(off.subagentRouting, "off")

        // Absent keys: empty strings, not nil (the row hides on empty model).
        let minimal = ClientStatus(dict: ["name": "codex"])!
        XCTAssertEqual(minimal.subagentModel, "")
        XCTAssertEqual(minimal.subagentRouting, "")

        // Unknown extra keys are tolerated (already true for the rest of
        // the struct; pin it for the subagent fields too).
        let extra = ClientStatus(dict: [
            "name": "claude-code", "subagent_model": "zai-org/GLM-5.2",
            "subagent_routing": "on", "future_key": 42,
        ])!
        XCTAssertEqual(extra.subagentModel, "zai-org/GLM-5.2")
        XCTAssertEqual(extra.subagentRouting, "on")
    }

    // subagentRowLabel formats the menubar row text.
    func testSubagentRowLabel() {
        XCTAssertEqual(subagentRowLabel(model: "claude-baseten-glm-5-2"),
                       "Subagents: claude-baseten-glm-5-2")
        XCTAssertEqual(subagentRowLabel(model: "zai-org/GLM-5.2"),
                       "Subagents: zai-org/GLM-5.2")
    }

    // Empty subagent_model hides the row: the menubar only renders the
    // subagent Toggle when c.subagentModel is non-empty. This is a pure
    // predicate test on the stored property the view checks.
    func testSubagentRowHiddenWhenModelEmpty() {
        let noModel = client("claude-code", route: "baseten")
        XCTAssertTrue(noModel.subagentModel.isEmpty)

        let withModel = clientWithSubagent("claude-code", route: "baseten",
                                           subagentModel: "claude-baseten-glm-5-2",
                                           subagentRouting: "on")
        XCTAssertFalse(withModel.subagentModel.isEmpty)
    }

    // Binding get semantics: routing "on" or empty (absent) means on;
    // "off" means off. Empty means on when a model is set (spec).
    func testSubagentRoutingBindingGet() {
        let on = clientWithSubagent("claude-code", route: "baseten",
                                    subagentModel: "m", subagentRouting: "on")
        let empty = clientWithSubagent("claude-code", route: "baseten",
                                       subagentModel: "m", subagentRouting: "")
        let off = clientWithSubagent("claude-code", route: "baseten",
                                     subagentModel: "m", subagentRouting: "off")
        XCTAssertTrue(on.subagentRouting != "off")
        XCTAssertTrue(empty.subagentRouting != "off")
        XCTAssertFalse(off.subagentRouting != "off")
    }

    // MARK: - families / model_catalog decoding

    private func clientWithFamilies(_ name: String, route: String = "baseten",
                                    families: [[String: Any]],
                                    catalog: [[String: Any]]) -> ClientStatus {
        ClientStatus(dict: [
            "name": name, "enabled": true, "bind_addr": "127.0.0.1:18081",
            "protocol_shape": "anthropic", "effective_route": route, "native_route": "anthropic",
            "auth_set": true, "currently_bound": true,
            "fallback": ["active": false],
            "families": families, "model_catalog": catalog,
        ])!
    }

    // families and model_catalog decode from the current admin status fields.
    func testClientStatusParsesFamiliesAndCatalog() {
        let c = clientWithFamilies("claude-code",
            families: [
                ["family": "opus", "configured_target": "native",
                 "configured_source": "explicit",
                 "effective_route": "anthropic", "effective_model": ""],
                ["family": "sonnet",
                 "configured_target": "claude-baseten-kimi-k2-7",
                 "configured_source": "explicit",
                 "effective_route": "baseten", "effective_model": "claude-baseten-kimi-k2-7"],
            ],
            catalog: [
                ["label": "GLM-5.2", "storage_target": "zai-org/GLM-5.2",
                 "slug": "zai-org/GLM-5.2", "alias": "claude-baseten-glm-5-2"],
                ["label": "Kimi-K2.7", "storage_target": "kai-org/Kimi-K2.7",
                 "slug": "kai-org/Kimi-K2.7", "alias": "claude-baseten-kimi-k2-7"],
            ])
        XCTAssertEqual(c.families.count, 2)
        XCTAssertEqual(c.families[0].family, "opus")
        XCTAssertEqual(c.families[0].configuredTarget, "native")
        XCTAssertEqual(c.families[0].effectiveRoute, "anthropic")
        XCTAssertEqual(c.families[0].effectiveModel, "")
        XCTAssertEqual(c.families[1].family, "sonnet")
        XCTAssertEqual(c.families[1].configuredTarget, "claude-baseten-kimi-k2-7")
        XCTAssertEqual(c.families[1].effectiveModel, "claude-baseten-kimi-k2-7")
        XCTAssertEqual(c.modelCatalog.count, 2)
        XCTAssertEqual(c.modelCatalog[0].label, "GLM-5.2")
        XCTAssertEqual(c.modelCatalog[0].target, "zai-org/GLM-5.2")
        XCTAssertEqual(c.modelCatalog[0].slug, "zai-org/GLM-5.2")
    }

    // Absent optional tables decode to empty arrays, not nil.
    func testClientStatusFamiliesAbsentIsEmpty() {
        let minimal = ClientStatus(dict: ["name": "codex"])!
        XCTAssertTrue(minimal.families.isEmpty)
        XCTAssertTrue(minimal.modelCatalog.isEmpty)
    }

    // Unknown extra fields inside a family/catalog entry are tolerated;
    // a malformed entry (missing the required key) is dropped, not fatal.
    func testClientStatusToleratesUnknownAndMalformedEntries() {
        let c = clientWithFamilies("claude-code",
            families: [
                ["family": "opus", "configured_target": "native", "future_key": 42],
                ["configured_target": "native"], // missing family -> dropped
            ],
            catalog: [
                ["storage_target": "zai-org/GLM-5.2", "slug": "zai-org/GLM-5.2",
                 "label": "GLM-5.2", "extra": true],
                ["label": "NoTarget"], // missing storage target -> dropped
            ])
        XCTAssertEqual(c.families.count, 1)
        XCTAssertEqual(c.families[0].family, "opus")
        XCTAssertEqual(c.modelCatalog.count, 1)
        XCTAssertEqual(c.modelCatalog[0].target, "zai-org/GLM-5.2")

        let removedFields = clientWithFamilies("claude-code",
            families: [["family": "opus", "pin": "native"]],
            catalog: [["target": "claude-baseten-glm-5-2",
                       "slug": "zai-org/GLM-5.2"]])
        XCTAssertNil(removedFields.families[0].configuredTarget)
        XCTAssertTrue(removedFields.modelCatalog.isEmpty)
    }

    // MARK: - familyRowLabel

    func testFamilyRowLabelEffectiveModel() {
        XCTAssertEqual(
            familyRowLabel(family: "opus", effectiveRoute: "baseten",
                           effectiveModel: "zai-org/GLM-5.2"),
            "Opus: zai-org/GLM-5.2")
    }

    func testFamilyRowLabelNativeFallback() {
        // effective_model empty: show the native route name.
        XCTAssertEqual(
            familyRowLabel(family: "opus", effectiveRoute: "anthropic",
                           effectiveModel: ""),
            "Opus: Native (anthropic)")
    }

    func testFamilyRowLabelDefaultFallback() {
        // Both empty: "default" so the row is never blank.
        XCTAssertEqual(
            familyRowLabel(family: "haiku", effectiveRoute: "",
                           effectiveModel: ""),
            "Haiku: default")
    }

    func testCapitalizeFamily() {
        XCTAssertEqual(capitalizeFamily("opus"), "Opus")
        XCTAssertEqual(capitalizeFamily("sonnet"), "Sonnet")
        XCTAssertEqual(capitalizeFamily(""), "")
        XCTAssertEqual(capitalizeFamily("Fable"), "Fable") // already capped
    }

    // MARK: - familyChoiceChecked

    func testFamilyChoiceCheckedNative() {
        XCTAssertTrue(familyChoiceChecked(pin: "native", choice: .native))
        XCTAssertFalse(familyChoiceChecked(pin: "", choice: .native))
        XCTAssertFalse(familyChoiceChecked(pin: "zai-org/GLM-5.2", choice: .native))
    }

    func testFamilyChoiceCheckedDefault() {
        XCTAssertTrue(familyChoiceChecked(pin: "", choice: .defaultMapping))
        XCTAssertFalse(familyChoiceChecked(pin: "native", choice: .defaultMapping))
        XCTAssertFalse(familyChoiceChecked(pin: "claude-baseten-glm-5-2", choice: .defaultMapping))
    }

    func testFamilyChoiceCheckedCatalogByTarget() {
        let entry = ModelCatalogEntry(dict: [
            "storage_target": "zai-org/GLM-5.2",
            "label": "GLM-5.2",
            "slug": "zai-org/GLM-5.2",
            "alias": "claude-baseten-glm-5-2",
        ])!
        XCTAssertTrue(familyChoiceChecked(pin: "zai-org/GLM-5.2",
                                          choice: .catalog(entry)))
        XCTAssertFalse(familyChoiceChecked(pin: "native",
                                           choice: .catalog(entry)))
    }

    func testFamilyChoiceCheckedCatalogBySlug() {
        let entry = ModelCatalogEntry(dict: [
            "storage_target": "zai-org/GLM-5.2",
            "label": "GLM-5.2",
            "slug": "zai-org/GLM-5.2",
            "alias": "claude-baseten-glm-5-2",
        ])!
        // pin is a raw slug -> matches slug.
        XCTAssertTrue(familyChoiceChecked(pin: "zai-org/GLM-5.2",
                                          choice: .catalog(entry)))
        XCTAssertFalse(familyChoiceChecked(pin: "other/slug",
                                           choice: .catalog(entry)))
    }

    func testRouterWindowFamilyPickerSelection() {
        let c = clientWithFamilies("claude-code",
            families: [
                ["family": "opus", "configured_target": "zai-org/GLM-5.2"],
                ["family": "sonnet", "configured_target": "native"],
                ["family": "haiku"],
                ["family": "fable", "configured_target": "custom/model"],
            ],
            catalog: [["label": "GLM-5.2", "storage_target": "zai-org/GLM-5.2",
                       "slug": "zai-org/GLM-5.2",
                       "alias": "claude-baseten-glm-5-2"]])

        XCTAssertEqual(familyPickerSelection(c.families[0], catalog: c.modelCatalog),
                       .catalog("zai-org/GLM-5.2"))
        XCTAssertEqual(familyPickerSelection(c.families[1], catalog: c.modelCatalog),
                       .native)
        XCTAssertEqual(familyPickerSelection(c.families[2], catalog: c.modelCatalog),
                       .defaultTarget)
        XCTAssertEqual(familyPickerSelection(c.families[3], catalog: c.modelCatalog),
                       .custom("custom/model"))
        XCTAssertEqual(familyChoice(.catalog("zai-org/GLM-5.2"),
                                    catalog: c.modelCatalog),
                       .catalog(c.modelCatalog[0]))
        XCTAssertNil(familyChoice(.custom("custom/model"), catalog: c.modelCatalog))
    }

    func testRouterWindowModelDisplayLabelFallbacks() {
        XCTAssertEqual(modelDisplayLabel(ModelCatalogEntry(dict: [
            "label": "GLM-5.2", "storage_target": "zai-org/GLM-5.2",
            "slug": "zai-org/GLM-5.2",
        ])!), "GLM-5.2")
        XCTAssertEqual(modelDisplayLabel(ModelCatalogEntry(dict: [
            "storage_target": "zai-org/GLM-5.2", "slug": "zai-org/GLM-5.2",
        ])!), "GLM-5.2")
        XCTAssertEqual(modelDisplayLabel(ModelCatalogEntry(dict: [
            "storage_target": "zai-org/GLM-5.2", "slug": "zai-org/GLM-5.2",
        ])!), "GLM-5.2")
    }

    func testRouterWindowFamilyLabelsUseCatalogDisplayName() {
        let catalog = [ModelCatalogEntry(dict: [
            "label": "GLM 5.2",
            "storage_target": "zai-org/GLM-5.2",
            "slug": "zai-org/GLM-5.2",
            "alias": "claude-baseten-glm-5-2",
        ])!]
        let family = FamilyEntry(dict: [
            "family": "opus",
            "configured_target": "zai-org/GLM-5.2",
            "configured_source": "default",
            "effective_route": "baseten",
            "effective_model": "zai-org/GLM-5.2",
        ])!
        let client = ClientStatus(dict: [
            "name": "claude-code",
            "enabled": true,
            "unmatched_native_model": [
                "configured_target": "zai-org/GLM-5.2",
            ],
            "model_catalog": [[
                "label": "GLM 5.2",
                "storage_target": "zai-org/GLM-5.2",
                "slug": "zai-org/GLM-5.2",
                "alias": "claude-baseten-glm-5-2",
            ]],
        ])!

        XCTAssertEqual(
            catalogModelDisplayLabel(
                "claude-baseten-glm-5-2",
                catalog: catalog),
            "GLM 5.2")
        XCTAssertEqual(
            familyConfiguredLabel(family, catalog: catalog),
            "Saved: Default · GLM 5.2")
        XCTAssertEqual(
            familyEffectiveStatus(
                family,
                globalRoutingEnabled: true,
                catalog: catalog),
            "Currently GLM 5.2")
        XCTAssertEqual(
            defaultFamilyOptionLabel(family, client: client),
            "Default · GLM 5.2")
    }

    // MARK: - familyDispatchArgs

    func testFamilyDispatchArgs() {
        let entry = ModelCatalogEntry(dict: [
            "storage_target": "zai-org/GLM-5.2",
            "label": "GLM-5.2",
            "slug": "zai-org/GLM-5.2",
            "alias": "claude-baseten-glm-5-2",
        ])!
        XCTAssertEqual(
            familyDispatchArgs(client: "claude-code", family: "opus", choice: .native),
            ["claude", "route", "opus", "native"])
        XCTAssertEqual(
            familyDispatchArgs(client: "claude-code", family: "opus",
                               choice: .defaultMapping),
            ["claude", "route", "opus", "default"])
        XCTAssertEqual(
            familyDispatchArgs(client: "claude-code", family: "opus",
                               choice: .catalog(entry)),
            ["claude", "route", "opus", "zai-org/GLM-5.2"])
    }

    // MARK: - subagentMenuRowLabel

    func testSubagentMenuRowLabel() {
        XCTAssertEqual(subagentMenuRowLabel(model: "claude-baseten-glm-5-2", routing: "on"),
                       "Subagents: claude-baseten-glm-5-2")
        // Empty routing means on when a model is set (spec).
        XCTAssertEqual(subagentMenuRowLabel(model: "claude-baseten-glm-5-2", routing: ""),
                       "Subagents: claude-baseten-glm-5-2")
        // Routing "off" is the compatibility wire value for leaving Claude
        // Code's requested subagent model untouched.
        XCTAssertEqual(subagentMenuRowLabel(model: "claude-baseten-glm-5-2", routing: "off"),
                       "Subagents: Claude Code model")
        // No model configured leaves Claude Code in control regardless of
        // the routing flag.
        XCTAssertEqual(subagentMenuRowLabel(model: "", routing: "on"),
                       "Subagents: Claude Code model")
        XCTAssertEqual(subagentMenuRowLabel(model: "", routing: ""),
                       "Subagents: Claude Code model")
    }

    func testSubagentRoutingDescriptionExplainsNoRewriteAndSavedOverride() {
        let noOverride = clientWithSubagent(
            "claude-code",
            route: "baseten",
            subagentModel: "zai-org/GLM-5.2",
            subagentRouting: "off")
        XCTAssertEqual(
            subagentRoutingDescription(
                client: noOverride,
                globalRoutingEnabled: true),
            "No subagent override. Claude Code chooses the model, then Baseten Switch applies that model’s family mapping or the default mapping.")
        XCTAssertEqual(
            subagentRoutingDescription(
                client: noOverride,
                globalRoutingEnabled: false),
            "No subagent override. Routing is Off, so native models requested by Claude Code use Anthropic.")

        let override = clientWithSubagent(
            "claude-code",
            route: "baseten",
            subagentModel: "moonshotai/Kimi-K2.5",
            subagentRouting: "on")
        XCTAssertEqual(
            subagentRoutingDescription(
                client: override,
                globalRoutingEnabled: true),
            "Overrides every detected subagent request to Kimi-K2.5 before routing.")
        XCTAssertEqual(
            subagentRoutingDescription(
                client: override,
                globalRoutingEnabled: false),
            "Routing is Off. Claude Code’s requested model uses Anthropic; saved override Kimi-K2.5 applies when routing is On.")

        let catalogOverride = ClientStatus(dict: [
            "name": "claude-code",
            "enabled": true,
            "subagent_model": "moonshotai/Kimi-K3",
            "subagent_routing": "on",
            "subagent_effective": "moonshotai/Kimi-K3",
            "model_catalog": [[
                "label": "Kimi K3",
                "storage_target": "moonshotai/Kimi-K3",
                "slug": "moonshotai/Kimi-K3",
                "available": true,
            ]],
        ])!
        XCTAssertEqual(
            subagentRoutingDescription(
                client: catalogOverride,
                globalRoutingEnabled: true),
            "Overrides every detected subagent request to Kimi K3 before routing.")
    }

    // MARK: - subagentChoiceChecked

    func testSubagentChoiceCheckedUsesClaudeCodeModel() {
        XCTAssertTrue(subagentChoiceChecked(subagentModel: "m", subagentRouting: "off",
                                            choice: .off))
        XCTAssertTrue(subagentChoiceChecked(subagentModel: "", subagentRouting: "",
                                            choice: .off))
        XCTAssertFalse(subagentChoiceChecked(subagentModel: "m", subagentRouting: "on",
                                             choice: .off))
    }

    func testSubagentChoiceCheckedCatalog() {
        let entry = ModelCatalogEntry(dict: [
            "storage_target": "zai-org/GLM-5.2",
            "label": "GLM-5.2",
            "slug": "zai-org/GLM-5.2",
            "alias": "claude-baseten-glm-5-2",
        ])!
        // routing on, model matches alias.
        XCTAssertTrue(subagentChoiceChecked(subagentModel: "claude-baseten-glm-5-2",
                                            subagentRouting: "on",
                                            choice: .catalog(entry)))
        // routing on, model matches slug.
        XCTAssertTrue(subagentChoiceChecked(subagentModel: "zai-org/GLM-5.2",
                                            subagentRouting: "",
                                            choice: .catalog(entry)))
        // Routing off is the compatibility wire value for no override.
        XCTAssertFalse(subagentChoiceChecked(subagentModel: "claude-baseten-glm-5-2",
                                             subagentRouting: "off",
                                             choice: .catalog(entry)))
        // model mismatch.
        XCTAssertFalse(subagentChoiceChecked(subagentModel: "other",
                                             subagentRouting: "on",
                                             choice: .catalog(entry)))
    }

    func testRouterWindowSubagentPickerSelection() {
        let catalog: [[String: Any]] = [
            ["label": "GLM-5.2", "storage_target": "zai-org/GLM-5.2",
             "slug": "zai-org/GLM-5.2", "alias": "claude-baseten-glm-5-2"],
        ]
        func make(_ model: String, _ routing: String) -> ClientStatus {
            ClientStatus(dict: [
                "name": "claude-code", "enabled": true, "effective_route": "baseten",
                "subagent_model": model, "subagent_routing": routing,
                "subagent_effective": routing == "off" ? "inherit" : model,
                "model_catalog": catalog,
            ])!
        }

        XCTAssertEqual(
            subagentPickerSelection(make("", "off")),
            .useClaudeCodeModel)
        XCTAssertEqual(subagentPickerSelection(make("zai-org/GLM-5.2", "on")),
                       .catalog("zai-org/GLM-5.2"))
        XCTAssertEqual(subagentPickerSelection(make("custom/model", "on")),
                       .custom("custom/model"))
        let client = make("zai-org/GLM-5.2", "on")
        XCTAssertEqual(subagentChoice(.catalog("zai-org/GLM-5.2"),
                                      catalog: client.modelCatalog),
                       .catalog(client.modelCatalog[0]))
        XCTAssertNil(subagentChoice(.custom("custom/model"),
                                    catalog: client.modelCatalog))
        XCTAssertEqual(
            subagentChoice(.useClaudeCodeModel, catalog: client.modelCatalog),
            .off)
    }

    // MARK: - subagentDispatchArgs

    func testSubagentDispatchArgs() {
        let entry = ModelCatalogEntry(dict: [
            "storage_target": "zai-org/GLM-5.2",
            "label": "GLM-5.2",
            "slug": "zai-org/GLM-5.2",
            "alias": "claude-baseten-glm-5-2",
        ])!
        XCTAssertEqual(
            subagentDispatchArgs(client: "claude-code", choice: .off),
            ["claude", "subagents", "inherit"])
        XCTAssertEqual(
            subagentDispatchArgs(client: "claude-code", choice: .catalog(entry)),
            ["claude", "subagents", "zai-org/GLM-5.2"])
    }
}
