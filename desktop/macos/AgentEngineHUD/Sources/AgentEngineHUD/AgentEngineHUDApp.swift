import AppKit
import SwiftUI

@main
struct AgentEngineHUDApp: App {
    @NSApplicationDelegateAdaptor(HUDAppDelegate.self) private var appDelegate

    var body: some Scene {
        Settings {
            EmptyView()
        }
    }
}

final class HUDAppDelegate: NSObject, NSApplicationDelegate {
    private var panel: FloatingPanel?

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.accessory)
        let panel = FloatingPanel()
        panel.contentView = NSHostingView(rootView: HUDRootView { [weak panel] expanded in
            panel?.setContentSize(expanded ? NSSize(width: 420, height: 540) : NSSize(width: 310, height: 82))
        })
        panel.orderFrontRegardless()
        self.panel = panel
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        true
    }
}

final class FloatingPanel: NSPanel {
    private var screenObserver: NSObjectProtocol?

    init() {
        let expanded = UserDefaults.standard.bool(forKey: "AgentEngineHUD.expanded")
        super.init(
            contentRect: NSRect(x: 0, y: 0, width: expanded ? 420 : 310, height: expanded ? 540 : 82),
            styleMask: [.nonactivatingPanel, .titled, .closable, .utilityWindow],
            backing: .buffered,
            defer: false
        )
        title = "Agent Engine"
        isFloatingPanel = true
        level = .floating
        hidesOnDeactivate = false
        becomesKeyOnlyIfNeeded = true
        collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary]
        isReleasedWhenClosed = false
        let hasSavedFrame = setFrameUsingName("AgentEngineHUD.FloatingPanel")
        setFrameAutosaveName("AgentEngineHUD.FloatingPanel")
        if !hasSavedFrame {
            center()
        }
        clampToVisibleScreen()
        screenObserver = NotificationCenter.default.addObserver(
            forName: NSApplication.didChangeScreenParametersNotification,
            object: nil,
            queue: .main
        ) { [weak self] _ in
            self?.clampToVisibleScreen()
        }
    }

    override var canBecomeKey: Bool { false }
    override var canBecomeMain: Bool { false }

    private func clampToVisibleScreen() {
        guard !NSScreen.screens.isEmpty else { return }
        let best = NSScreen.screens.max { first, second in
            intersectionArea(frame, first.visibleFrame) < intersectionArea(frame, second.visibleFrame)
        } ?? NSScreen.screens[0]
        let visible = best.visibleFrame
        var restored = frame
        restored.origin.x = min(max(restored.origin.x, visible.minX), visible.maxX - restored.width)
        restored.origin.y = min(max(restored.origin.y, visible.minY), visible.maxY - restored.height)
        setFrame(restored, display: false)
    }

    private func intersectionArea(_ first: NSRect, _ second: NSRect) -> CGFloat {
        let intersection = first.intersection(second)
        return intersection.isNull ? 0 : intersection.width * intersection.height
    }

    deinit {
        if let screenObserver {
            NotificationCenter.default.removeObserver(screenObserver)
        }
    }
}
