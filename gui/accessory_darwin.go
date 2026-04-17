package main

/*
#cgo darwin CFLAGS: -x objective-c
#cgo darwin LDFLAGS: -framework Cocoa

#import <Cocoa/Cocoa.h>

static void setAccessoryPolicy(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
    });
}
*/
import "C"

// makeAccessoryApp flips NSApp's activation policy to Accessory on the
// main queue. Wails v2's AppDelegate calls setActivationPolicy:Regular
// during startup, which overrides Info.plist's LSUIElement. Running
// this after Wails is up restores the "menu-bar-only" behavior.
func makeAccessoryApp() {
	C.setAccessoryPolicy()
}
