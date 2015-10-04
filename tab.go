package autogcd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/wirepair/gcd"
	"github.com/wirepair/gcd/gcdapi"
	"log"
	"sync"
	"time"
)

// When we are unable to find an element/nodeId
type ElementNotFoundErr struct {
	Message string
}

func (e *ElementNotFoundErr) Error() string {
	return "Unable to find element " + e.Message
}

// When we are unable to access a tab
type InvalidTabErr struct {
	Message string
}

func (e *InvalidTabErr) Error() string {
	return "Unable to access tab: " + e.Message
}

// When unable to navigate Forward or Back
type InvalidNavigationErr struct {
	Message string
}

func (e *InvalidNavigationErr) Error() string {
	return e.Message
}

// Returned when an injected script caused an error
type ScriptEvaluationErr struct {
	Message          string
	ExceptionText    string
	ExceptionDetails *gcdapi.DebuggerExceptionDetails
}

func (e *ScriptEvaluationErr) Error() string {
	return e.Message + " " + e.ExceptionText
}

// When Tab.Navigate has timed out
type TimeoutErr struct {
	Message string
}

func (e *TimeoutErr) Error() string {
	return "Timed out " + e.Message
}

type GcdResponseFunc func(target *gcd.ChromeTarget, payload []byte)

// A function to handle javascript dialog prompts as they occur, pass to SetJavaScriptPromptHandler
// Internally this should call tab.Page.HandleJavaScriptDialog(accept bool, promptText string)
type PromptHandlerFunc func(tab *Tab, message, promptType string)

// A function for handling console messages
type ConsoleMessageFunc func(tab *Tab, message *gcdapi.ConsoleConsoleMessage)

// A function for handling network requests
type NetworkRequestHandlerFunc func(tab *Tab, request *NetworkRequest)

// A function for handling network responses
type NetworkResponseHandlerFunc func(tab *Tab, response *NetworkResponse)

// A function for ListenStorageEvents returns the eventType of cleared, updated, removed or added.
type StorageFunc func(tab *Tab, eventType string, eventDetails *StorageEvent)

// A function to listen for DOM Node Change Events
type DomChangeHandlerFunc func(tab *Tab, change *NodeChangeEvent)

// A function to iteratively call until returns without error
type ConditionalFunc func(tab *Tab) bool

// Our tab object for driving a specific tab and gathering elements.
type Tab struct {
	*gcd.ChromeTarget                        // underlying chrometarget
	eleMutex           *sync.RWMutex         // locks our elements when added/removed.
	elements           map[int]*Element      // our map of elements for this tab
	topNodeId          int                   // the nodeId of the current top level #document
	topFrameId         string                // the frameId of the current top level #document
	isNavigating       bool                  // are we currently navigating (between Page.Navigate -> page.loadEventFired)
	isTransitioning    bool                  // has navigation occurred on the top frame (not due to Navigate() being called)
	debug              bool                  // for debug printing
	nodeChange         chan *NodeChangeEvent // for receiving node change events from tab_subscribers
	navigationCh       chan int              // for receiving navigation complete messages while isNavigating is true
	docUpdateCh        chan struct{}         // for receiving document update completion while isNavigating is true
	setNodesWg         *sync.WaitGroup       // tracks setChildNode event calls when Navigating.
	navigationTimeout  time.Duration         // amount of time to wait before failing navigation
	elementTimeout     time.Duration         // amount of time to wait for element readiness
	stabilityTimeout   time.Duration         // amount of time to give up waiting for stability
	stableAfter        time.Duration         // amount of time of no activity to consider the DOM stable
	lastNodeChangeTime time.Time             // timestamp of when the last node change occurred
	domChangeHandler   DomChangeHandlerFunc  // allows the caller to be notified of DOM change events.
}

// Creates a new tab using the underlying ChromeTarget
func NewTab(target *gcd.ChromeTarget) *Tab {
	t := &Tab{ChromeTarget: target}
	t.eleMutex = &sync.RWMutex{}
	t.elements = make(map[int]*Element)
	t.nodeChange = make(chan *NodeChangeEvent)
	t.isNavigating = false                 // for when Tab.Navigate() is called
	t.isTransitioning = false              // for when an action or redirect causes the top level frame to navigate
	t.navigationCh = make(chan int, 1)     // for signaling navigation complete
	t.setNodesWg = &sync.WaitGroup{}       // wait for setChildNode events to complete
	t.docUpdateCh = make(chan struct{})    // wait for documentUpdate to be called during navigation
	t.navigationTimeout = 30 * time.Second // default 30 seconds for timeout
	t.elementTimeout = 5 * time.Second     // default 5 seconds for waiting for element.
	t.stabilityTimeout = 2 * time.Second   // default 2 seconds before we give up waiting for stability
	t.stableAfter = 300 * time.Millisecond // default 300 ms for considering the DOM stable
	t.domChangeHandler = nil
	t.Page.Enable()
	t.DOM.Enable()
	t.Console.Enable()
	t.Debugger.Enable()
	t.subscribeEvents()
	go t.listenDOMChanges()
	return t
}

// Enable or disable internal debug printing
func (t *Tab) Debug(enabled bool) {
	t.debug = enabled
}

// How long to wait in seconds for navigations before giving up, default is 30 seconds
func (t *Tab) SetNavigationTimeout(timeout time.Duration) {
	t.navigationTimeout = timeout
}

// How long to wait in seconds for ele.WaitForReady() before giving up, default is 5 seconds
func (t *Tab) SetElementWaitTimeout(timeout time.Duration) {
	t.elementTimeout = timeout
}

// How long to wait for WaitStable() to return, default is 2 seconds.
func (t *Tab) SetStabilityTimeout(timeout time.Duration) {
	t.stabilityTimeout = timeout
}

// How long to wait for no node changes before we consider the DOM stable.
// Note that stability timeout will fire if the DOM is constantly changing.
// The deafult stableAfter is 300 ms.
func (t *Tab) SetStabilityTime(stableAfter time.Duration) {
	t.stableAfter = stableAfter
}

// Navigates to a URL and does not return until the Page.loadEventFired event
// as well as all setChildNode events have completed.
// Returns the frameId of the Tab that this navigation occured on or error.
func (t *Tab) Navigate(url string) (string, error) {
	if t.isNavigating == true {
		return "", &InvalidNavigationErr{Message: "Unable to navigate, already navigating."}
	}
	t.isNavigating = true
	defer func() {
		t.isNavigating = false
	}()

	frameId, err := t.Page.Navigate(url)
	if err != nil {
		return "", err
	}

	err = t.navigationWait(url)
	if err != nil {
		return frameId, err
	}
	err = t.documentWait(url)
	t.debugf("navigation complete")
	return frameId, err
}

// Wait for Page.loadEventFired or timeout.
func (t *Tab) navigationWait(url string) error {
	timeoutTimer := time.NewTimer(t.navigationTimeout)

	select {
	case <-t.navigationCh:
		timeoutTimer.Stop()
	case <-timeoutTimer.C:
		return &TimeoutErr{Message: "navigating to: " + url}
	}
	return nil
}

// Wait for document updated event from Tab.documentUpdated and setChildNode event processing
// to finish so we have a valid set of elements. Timeout after two seconds if we never get a
// document update event.
func (t *Tab) documentWait(url string) error {
	timeoutTimer := time.NewTimer(2 * time.Second)
	select {
	case <-t.docUpdateCh:
		timeoutTimer.Stop()
	case <-timeoutTimer.C:
		return &TimeoutErr{Message: "waiting for document updated failed for: " + url}
	}

	// wait for setChildNode events to complete.
	t.setNodesWg.Wait()
	return nil
}

// Returns true if we are transitioning to a new page. This is not set when Navigate is called.
func (t *Tab) IsTransitioning() bool {
	return t.isTransitioning
}

// Returns the current navigation index, history entries or error
func (t *Tab) NavigationHistory() (int, []*gcdapi.PageNavigationEntry, error) {
	return t.Page.GetNavigationHistory()
}

// Reloads the page injecting evalScript to run on load. set Ignore cache to true
// to have it act like ctrl+f5.
func (t *Tab) Reload(ignoreCache bool, evalScript string) error {
	_, err := t.Page.Reload(ignoreCache, evalScript)
	return err
}

// Looks up the next navigation entry from the history and navigates to it.
// Returns error if we could not find the next entry or navigation failed
func (t *Tab) Forward() error {
	next, err := t.ForwardEntry()
	if err != nil {
		return err
	}
	_, err = t.Page.NavigateToHistoryEntry(next.Id)
	return err
}

// Returns the next entry in our navigation history for this tab.
func (t *Tab) ForwardEntry() (*gcdapi.PageNavigationEntry, error) {
	idx, entries, err := t.NavigationHistory()
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(entries); i++ {
		if idx < entries[i].Id {
			return entries[i], nil
		}
	}
	return nil, &InvalidNavigationErr{Message: "Unable to navigate forward as we are on the latest navigation entry"}
}

// Looks up the previous navigation entry from the history and navigates to it.
// Returns error if we could not find the previous entry or navigation failed
func (t *Tab) Back() error {
	prev, err := t.BackEntry()
	if err != nil {
		return err
	}
	_, err = t.Page.NavigateToHistoryEntry(prev.Id)
	return err
}

// Returns the previous entry in our navigation history for this tab.
func (t *Tab) BackEntry() (*gcdapi.PageNavigationEntry, error) {
	idx, entries, err := t.NavigationHistory()
	if err != nil {
		return nil, err
	}
	for i := len(entries); i > 0; i-- {
		if idx < entries[i].Id {
			return entries[i], nil
		}
	}
	return nil, &InvalidNavigationErr{Message: "Unable to navigate backward as we are on the first navigation entry"}
}

// Calls a function every rate until conditionFn returns true or timeout occurs.
func (t *Tab) WaitFor(rate, timeout time.Duration, conditionFn ConditionalFunc) error {
	rateTicker := time.NewTicker(rate)
	timeoutTimer := time.NewTimer(timeout)
	for {
		select {
		case <-timeoutTimer.C:
			return &TimeoutErr{Message: "waiting for conditional func to return true"}
		case <-rateTicker.C:
			ret := conditionFn(t)
			if ret == true {
				timeoutTimer.Stop()
				return nil
			}
		}
	}
}

// A very rudementary stability check, compare current time with lastNodeChangeTime and see if it
// is greater than the stableAfter duration. If it is, that means we haven't seen any activity over the minimum
// allowed time, in which case we consider the DOM stable. Note this will most likely not work for sites
// that insert and remove elements on timer/intervals as it will constantly update our lastNodeChangeTime
// value. However, for most cases this should be enough. This should only be necessary to call when
// a navigation event occurs under the page's control (not a direct tab.Navigate) call. Common examples
// would be submitting an XHR based form that does a history.pushState and does *not* actually load a new
// page but simply inserts and removes elements dynamically. Returns error only if we timed out.
func (t *Tab) WaitStable() error {
	checkRate := 20 * time.Millisecond
	timeoutTimer := time.NewTimer(t.stabilityTimeout)
	if t.stableAfter < checkRate {
		checkRate = t.stableAfter / 2 // halve the checkRate of the user supplied stabilityTime

	}
	stableCheck := time.NewTicker(checkRate) // check last node change every 20 seconds
	for {
		select {
		case <-timeoutTimer.C:
			return &TimeoutErr{Message: "waiting for DOM stability"}
		case <-stableCheck.C:
			//log.Printf("stable check tick, lastnode change time %v", time.Now().Sub(t.lastNodeChangeTime))
			if time.Now().Sub(t.lastNodeChangeTime) >= t.stableAfter {
				//log.Printf("times up!")
				return nil
			}

		}
	}
	return nil
}

// Returns the source of a script by its scriptId.
func (t *Tab) GetScriptSource(scriptId string) (string, error) {
	return t.Debugger.GetScriptSource(scriptId)
}

// Gets the top document and updates our list of elements DO NOT CALL DOM.GetDocument after
// the page has loaded, it creates a new nodeId and all functions that look up elements (QuerySelector)
// will fail.
func (t *Tab) getDocument() (*Element, error) {
	doc, err := t.DOM.GetDocument()
	if err != nil {
		return nil, err
	}
	t.topNodeId = doc.NodeId
	t.topFrameId = doc.FrameId
	t.debugf("got top nodeId %d", t.topNodeId)
	t.addNodes(doc)
	eleDoc, _ := t.getElement(doc.NodeId)
	return eleDoc, nil
}

func (t *Tab) GetDocument() (*Element, error) {
	docEle, ok := t.getElement(t.topNodeId)
	if !ok {
		return nil, &ElementNotFoundErr{Message: "top document node id not found."}
	}
	return docEle, nil
}

// Returns either an element from our list of ready/known nodeIds or a new un-ready element
// If it's not ready we return false. Note this does have a side effect of adding a potentially
// invalid element to our list of known elements. But it is assumed this method will be called
// with a valid nodeId that chrome has not informed us about yet. Once we are informed, we need
// to update it via our list and not some reference that could disappear.
func (t *Tab) GetElementByNodeId(nodeId int) (*Element, bool) {
	t.eleMutex.RLock()
	ele, ok := t.elements[nodeId]
	t.eleMutex.RUnlock()
	if ok {
		return ele, true
	}
	newEle := newElement(t, nodeId)
	t.eleMutex.Lock()
	t.elements[nodeId] = newEle // add non-ready element to our list.
	t.eleMutex.Unlock()
	return newEle, false
}

// Returns the element by searching the top level document for an element with attributeId
// Does not work on frames.
func (t *Tab) GetElementById(attributeId string) (*Element, bool, error) {
	return t.GetDocumentElementById(t.topNodeId, attributeId)
}

// Returns an element from a specific Document.
func (t *Tab) GetDocumentElementById(docNodeId int, attributeId string) (*Element, bool, error) {
	var err error

	docNode, ok := t.getElement(docNodeId)
	if !ok {
		return nil, false, &ElementNotFoundErr{Message: fmt.Sprintf("docNodeId %s not found", docNodeId)}
	}

	selector := "#" + attributeId

	nodeId, err := t.DOM.QuerySelector(docNode.id, selector)
	if err != nil {
		return nil, false, err
	}
	ele, ready := t.GetElementByNodeId(nodeId)
	return ele, ready, nil
}

// Get all elements that match a selector from the top level document
func (t *Tab) GetElementsBySelector(selector string) ([]*Element, error) {
	return t.GetDocumentElementsBySelector(t.topNodeId, selector)
}

// Gets all elements of a child
func (t *Tab) GetChildElements(element *Element) []*Element {
	return t.GetChildElementsOfType(element, "*")
}

func (t *Tab) GetChildElementsOfType(element *Element, tagType string) []*Element {
	elements := make([]*Element, 0)
	t.recursivelyGetChildren(element.node.Children, &elements, tagType)
	log.Printf("DONE GOT: %d for %s\n", len(elements), tagType)
	return elements
}

func (t *Tab) recursivelyGetChildren(children []*gcdapi.DOMNode, elements *[]*Element, tagType string) {
	//log.Printf("recursivelyGetchildren %d children", len(children))
	for _, child := range children {
		ele, ready := t.GetElementByNodeId(child.NodeId)
		// only add if it's ready and tagType matches or tagType is *
		if ready == true && (tagType == "*" || tagType == ele.nodeName) {
			*elements = append(*elements, ele)
		}
		// not ready, or doesn't have children
		if ready == false || ele.node.Children == nil || len(ele.node.Children) == 0 {
			continue
		}
		t.recursivelyGetChildren(ele.node.Children, elements, tagType)
	}
}

// Same as GetChildElementsBySelector
func (t *Tab) GetDocumentElementsBySelector(docNodeId int, selector string) ([]*Element, error) {
	docNode, ok := t.getElement(docNodeId)
	if !ok {
		return nil, &ElementNotFoundErr{Message: fmt.Sprintf("docNodeId %s not found", docNodeId)}
	}
	nodeIds, errQuery := t.DOM.QuerySelectorAll(docNode.id, selector)
	if errQuery != nil {
		return nil, errQuery
	}

	elements := make([]*Element, len(nodeIds))

	for k, nodeId := range nodeIds {
		elements[k], _ = t.GetElementByNodeId(nodeId)
	}

	return elements, nil
}

// Returns the documents source, as visible, if docId is 0, returns top document source.
func (t *Tab) GetPageSource(docNodeId int) (string, error) {
	if docNodeId == 0 {
		docNodeId = t.topNodeId
	}
	doc, ok := t.getElement(docNodeId)
	if !ok {
		return "", &ElementNotFoundErr{Message: fmt.Sprintf("docNodeId %d not found", docNodeId)}
	}
	return t.DOM.GetOuterHTML(doc.id)
}

// Returns the current url of the top level document
func (t *Tab) GetCurrentUrl() (string, error) {
	return t.GetDocumentCurrentUrl(t.topNodeId)
}

// Returns the current url of the provided docNodeId
func (t *Tab) GetDocumentCurrentUrl(docNodeId int) (string, error) {
	docNode, ok := t.getElement(docNodeId)
	if !ok {
		return "", &ElementNotFoundErr{Message: fmt.Sprintf("docNodeId %d not found", docNodeId)}
	}
	return docNode.node.DocumentURL, nil
}

// Issues a left button mousePressed then mouseReleased on the x, y coords provided.
func (t *Tab) Click(x, y int) error {
	return t.click(x, y, 1)
}

func (t *Tab) click(x, y, clickCount int) error {
	// "mousePressed", "mouseReleased", "mouseMoved"
	// enum": ["none", "left", "middle", "right"]
	pressed := "mousePressed"
	released := "mouseReleased"
	modifiers := 0
	timestamp := 0.0
	button := "left"
	time.Now().Second()
	if _, err := t.Input.DispatchMouseEvent(pressed, x, y, modifiers, timestamp, button, clickCount); err != nil {
		return err
	}

	if _, err := t.Input.DispatchMouseEvent(released, x, y, modifiers, timestamp, button, clickCount); err != nil {
		return err
	}
	return nil
}

func (t *Tab) DoubleClick(x, y int) error {
	return t.click(x, y, 2)
}

func (t *Tab) MoveMouse(x, y int) error {
	_, err := t.Input.DispatchMouseEvent("mouseMoved", x, y, 0, 0.0, "none", 0)
	return err
}

// Sends keystrokes to whatever is focused, best called from Element.SendKeys which will
// try to focus on the element first. Use \n for Enter, \b for backspace or \t for Tab.
func (t *Tab) SendKeys(text string) error {
	theType := "char"
	modifiers := 0
	timestamp := 0.0
	unmodifiedText := ""
	keyIdentifier := ""
	code := ""
	key := ""
	windowsVirtualKeyCode := 0
	nativeVirtualKeyCode := 0
	autoRepeat := false
	isKeypad := false
	isSystemKey := false

	// loop over input, looking for system keys and handling them
	for _, inputchar := range text {
		input := string(inputchar)

		// check system keys
		switch input {
		case "\r", "\n", "\t", "\b":
			if err := t.pressSystemKey(input); err != nil {
				return err
			}
			continue
		}
		_, err := t.Input.DispatchKeyEvent(theType, modifiers, timestamp, input, unmodifiedText, keyIdentifier, code, key, windowsVirtualKeyCode, nativeVirtualKeyCode, autoRepeat, isKeypad, isSystemKey)
		if err != nil {
			return err
		}
	}
	return nil
}

// Super ghetto, i know.
func (t *Tab) pressSystemKey(systemKey string) error {
	systemKeyCode := 0
	keyIdentifier := ""
	switch systemKey {
	case "\b":
		keyIdentifier = "Backspace"
		systemKeyCode = 8
	case "\t":
		keyIdentifier = "Tab"
		systemKeyCode = 9
	case "\r", "\n":
		systemKey = "\r"
		keyIdentifier = "Enter"
		systemKeyCode = 13
	}

	modifiers := 0
	timestamp := 0.0
	unmodifiedText := ""
	autoRepeat := false
	isKeypad := false
	isSystemKey := false
	if _, err := t.Input.DispatchKeyEvent("rawKeyDown", modifiers, timestamp, systemKey, systemKey, keyIdentifier, keyIdentifier, "", systemKeyCode, systemKeyCode, autoRepeat, isKeypad, isSystemKey); err != nil {
		return err
	}
	if _, err := t.Input.DispatchKeyEvent("char", modifiers, timestamp, systemKey, unmodifiedText, "", "", "", 0, 0, autoRepeat, isKeypad, isSystemKey); err != nil {
		return err
	}
	return nil
}

// Injects custom javascript prior to the page loading on all frames. Returns scriptId which can be
// used to remove the script. Since this loads on all frames, if you only want the script to interact with the
// top document, you'll need to do checks in the injected script such as testing location.href.
// Alternatively, you can use Tab.EvaluateScript to only work on the global context.
func (t *Tab) InjectScriptOnLoad(scriptSource string) (string, error) {
	scriptId, err := t.Page.AddScriptToEvaluateOnLoad(scriptSource)
	if err != nil {
		return "", err
	}
	return scriptId, nil
}

// Removes the script by the scriptId.
func (t *Tab) RemoveScriptFromOnLoad(scriptId string) error {
	_, err := t.Page.RemoveScriptToEvaluateOnLoad(scriptId)
	return err
}

// Evaluates script in the global context.
func (t *Tab) EvaluateScript(scriptSource string) (*gcdapi.RuntimeRemoteObject, error) {
	objectGroup := "autogcd"
	includeCommandLineAPI := true
	doNotPauseOnExceptionsAndMuteConsole := true
	contextId := 0
	returnByValue := true
	generatePreview := true
	rro, thrown, exception, err := overridenRuntimeEvaluate(t.ChromeTarget, scriptSource, objectGroup, includeCommandLineAPI, doNotPauseOnExceptionsAndMuteConsole, contextId, returnByValue, generatePreview)
	if err != nil {
		return nil, err
	}
	if thrown || exception != nil {
		return nil, &ScriptEvaluationErr{Message: "error executing script: ", ExceptionText: exception.Text, ExceptionDetails: exception}
	}
	return rro, nil
}

// Takes a screenshot of the currently loaded page (only the dimensions visible in browser window)
func (t *Tab) GetScreenShot() ([]byte, error) {
	var imgBytes []byte
	img, err := t.Page.CaptureScreenshot()
	if err != nil {
		return nil, err
	}
	imgBytes, err = base64.StdEncoding.DecodeString(img)
	if err != nil {
		return nil, err
	}
	return imgBytes, nil
}

// Returns the top document title
func (t *Tab) GetTitle() (string, error) {
	resp, err := t.EvaluateScript("window.top.document.title")
	if err != nil {
		return "", err
	}
	return resp.Value, nil
}

// Returns the raw source (non-serialized DOM) of the frame. If you want the visible
// source, call GetPageSource, passing in the frame's nodeId. Make sure you wait for
// the element's WaitForReady() to return without error first.
func (t *Tab) GetFrameSource(frameId, url string) (string, bool, error) {
	return t.Page.GetResourceContent(frameId, url)
}

// Gets all frame ids and urls from the top level document.
func (t *Tab) GetFrameResources() (map[string]string, error) {
	resources, err := t.Page.GetResourceTree()
	if err != nil {
		return nil, err
	}
	resourceMap := make(map[string]string)
	resourceMap[resources.Frame.Id] = resources.Frame.Url
	recursivelyGetFrameResource(resourceMap, resources)
	return resourceMap, nil
}

// Iterate over frame resources and return a map of id => urls
func recursivelyGetFrameResource(resourceMap map[string]string, resource *gcdapi.PageFrameResourceTree) {
	for _, frame := range resource.ChildFrames {
		resourceMap[frame.Frame.Id] = frame.Frame.Url
		recursivelyGetFrameResource(resourceMap, frame)
	}
}

// Returns all documents as elements that are known.
func (t *Tab) GetFrameDocuments() []*Element {
	frames := make([]*Element, 0)
	t.eleMutex.RLock()
	for _, ele := range t.elements {
		if ok, _ := ele.IsDocument(); ok {
			frames = append(frames, ele)
		}
	}
	t.eleMutex.RUnlock()
	return frames
}

// Returns the cookies from the tab.
func (t *Tab) GetCookies() ([]*gcdapi.NetworkCookie, error) {
	return t.Page.GetCookies()
}

// Deletes the cookie from the browser
func (t *Tab) DeleteCookie(cookieName, url string) error {
	_, err := t.Page.DeleteCookie(cookieName, url)
	return err
}

// Override the user agent for requests going out.
func (t *Tab) SetUserAgent(userAgent string) error {
	_, err := t.Network.SetUserAgentOverride(userAgent)
	return err
}

// Registers chrome to start retrieving console messages, caller must pass in call back
// function to handle it.
func (t *Tab) GetConsoleMessages(messageHandler ConsoleMessageFunc) {
	t.Subscribe("Console.messageAdded", t.defaultConsoleMessageAdded(messageHandler))
}

// Stops the debugger service from sending console messages and closes the channel
// Pass shouldDisable as true if you wish to disable Console debugger
func (t *Tab) StopConsoleMessages(shouldDisable bool) error {
	var err error
	t.Unsubscribe("Console.messageAdded")
	if shouldDisable {
		_, err = t.Console.Disable()
	}
	return err
}

// Listens to network traffic, either handler can be nil in which case we'll only call the handler defined.
func (t *Tab) GetNetworkTraffic(requestHandlerFn NetworkRequestHandlerFunc, responseHandlerFn NetworkResponseHandlerFunc) error {
	if requestHandlerFn == nil && responseHandlerFn == nil {
		return nil
	}
	_, err := t.Network.Enable()
	if err != nil {
		return err
	}

	if requestHandlerFn != nil {
		t.Subscribe("Network.requestWillBeSent", func(target *gcd.ChromeTarget, payload []byte) {
			message := &gcdapi.NetworkRequestWillBeSentEvent{}
			if err := json.Unmarshal(payload, message); err == nil {
				p := message.Params
				request := &NetworkRequest{RequestId: p.RequestId, FrameId: p.FrameId, LoaderId: p.LoaderId, DocumentURL: p.DocumentURL, Request: p.Request, Timestamp: p.Timestamp, Initiator: p.Initiator, RedirectResponse: p.RedirectResponse, Type: p.Type}
				requestHandlerFn(t, request)
			}
		})
	}

	if responseHandlerFn != nil {
		t.Subscribe("Network.responseReceived", func(target *gcd.ChromeTarget, payload []byte) {
			message := &gcdapi.NetworkResponseReceivedEvent{}
			if err := json.Unmarshal(payload, message); err == nil {
				p := message.Params
				response := &NetworkResponse{RequestId: p.RequestId, FrameId: p.FrameId, LoaderId: p.LoaderId, Response: p.Response, Timestamp: p.Timestamp, Type: p.Type}
				responseHandlerFn(t, response)
			}
		})
	}
	return nil
}

// Unsubscribes from network request/response events and disables the Network debugger.
// Pass shouldDisable as true if you wish to disable the network service. (NOT RECOMMENDED)
func (t *Tab) StopNetworkTraffic(shouldDisable bool) error {
	var err error
	t.Unsubscribe("Network.requestWillBeSent")
	t.Unsubscribe("Network.responseReceived")
	if shouldDisable {
		_, err = t.Network.Disable()
	}
	return err
}

// Listens for storage events, storageFn should switch on type of cleared, removed, added or updated.
// cleared holds IsLocalStorage and SecurityOrigin values only.
// removed contains above plus Key.
// added contains above plus NewValue.
// updated contains above plus OldValue.
func (t *Tab) GetStorageEvents(storageFn StorageFunc) error {
	_, err := t.DOMStorage.Enable()
	if err != nil {
		return err
	}
	t.Subscribe("Storage.domStorageItemsCleared", func(target *gcd.ChromeTarget, payload []byte) {
		message := &gcdapi.DOMStorageDomStorageItemsClearedEvent{}
		if err := json.Unmarshal(payload, message); err == nil {
			p := message.Params
			storageEvent := &StorageEvent{IsLocalStorage: p.StorageId.IsLocalStorage, SecurityOrigin: p.StorageId.SecurityOrigin}
			storageFn(t, "cleared", storageEvent)
		}
	})
	t.Subscribe("Storage.domStorageItemRemoved", func(target *gcd.ChromeTarget, payload []byte) {
		message := &gcdapi.DOMStorageDomStorageItemRemovedEvent{}
		if err := json.Unmarshal(payload, message); err == nil {
			p := message.Params
			storageEvent := &StorageEvent{IsLocalStorage: p.StorageId.IsLocalStorage, SecurityOrigin: p.StorageId.SecurityOrigin, Key: p.Key}
			storageFn(t, "removed", storageEvent)
		}
	})
	t.Subscribe("Storage.domStorageItemAdded", func(target *gcd.ChromeTarget, payload []byte) {
		message := &gcdapi.DOMStorageDomStorageItemAddedEvent{}
		if err := json.Unmarshal(payload, message); err == nil {
			p := message.Params
			storageEvent := &StorageEvent{IsLocalStorage: p.StorageId.IsLocalStorage, SecurityOrigin: p.StorageId.SecurityOrigin, Key: p.Key, NewValue: p.NewValue}
			storageFn(t, "added", storageEvent)
		}
	})
	t.Subscribe("Storage.domStorageItemUpdated", func(target *gcd.ChromeTarget, payload []byte) {
		message := &gcdapi.DOMStorageDomStorageItemUpdatedEvent{}
		if err := json.Unmarshal(payload, message); err == nil {
			p := message.Params
			storageEvent := &StorageEvent{IsLocalStorage: p.StorageId.IsLocalStorage, SecurityOrigin: p.StorageId.SecurityOrigin, Key: p.Key, NewValue: p.NewValue, OldValue: p.OldValue}
			storageFn(t, "updated", storageEvent)
		}
	})
	return nil
}

// Stops listening for storage events, set shouldDisable to true if you wish to disable DOMStorage debugging.
func (t *Tab) StopStorageEvents(shouldDisable bool) error {
	var err error
	t.Unsubscribe("Storage.domStorageItemsCleared")
	t.Unsubscribe("Storage.domStorageItemRemoved")
	t.Unsubscribe("Storage.domStorageItemAdded")
	t.Unsubscribe("Storage.domStorageItemUpdated")

	if shouldDisable {
		_, err = t.DOMStorage.Disable()
	}
	return err
}

// Set a handler for javascript prompts, most likely you should call tab.Page.HandleJavaScriptDialog(accept bool, msg string)
// to actually handle the prompt, otherwise the tab will be blocked waiting for input and never additional events.
func (t *Tab) SetJavaScriptPromptHandler(promptHandlerFn PromptHandlerFunc) {
	t.Subscribe("Page.javascriptDialogOpening", func(target *gcd.ChromeTarget, payload []byte) {
		message := &gcdapi.PageJavascriptDialogOpeningEvent{}
		if err := json.Unmarshal(payload, message); err == nil {
			promptHandlerFn(t, message.Params.Message, message.Params.Type)
		}
	})
}

// Allow the caller to be notified of DOM NodeChangeEvents. Simply call this with a nil function handler to stop
// receiving dom event changes.
func (t *Tab) GetDOMChanges(domHandlerFn DomChangeHandlerFunc) {
	t.domChangeHandler = domHandlerFn
}

// handles console messages coming in, responds by calling call back function
func (t *Tab) defaultConsoleMessageAdded(fn ConsoleMessageFunc) GcdResponseFunc {
	return func(target *gcd.ChromeTarget, payload []byte) {
		message := &gcdapi.ConsoleMessageAddedEvent{}
		err := json.Unmarshal(payload, message)
		if err == nil {
			// call the callback handler
			fn(t, message.Params.Message)
		}
	}
}

// see tab_subscribers.go
func (t *Tab) subscribeEvents() {
	// DOM Related

	t.subscribeSetChildNodes()
	t.subscribeAttributeModified()
	t.subscribeAttributeRemoved()
	t.subscribeCharacterDataModified()
	t.subscribeChildNodeCountUpdated()
	t.subscribeChildNodeInserted()
	t.subscribeChildNodeRemoved()
	// This doesn't seem useful.
	// t.subscribeInlineStyleInvalidated()
	t.subscribeDocumentUpdated()

	// Navigation Related
	t.subscribeLoadEvent()
	t.subscribeFrameLoadingEvent()
	t.subscribeFrameFinishedEvent()
}

// listens for NodeChangeEvents and dispatches them accordingly. Calls the user
// defined domChangeHandler if bound. Updates the lastNodeChangeTime to the current
// time.
func (t *Tab) listenDOMChanges() {
	for {
		select {
		case nodeChangeEvent := <-t.nodeChange:
			t.debugf("%s\n", nodeChangeEvent.EventType)
			t.handleNodeChange(nodeChangeEvent)
			// if the caller registered a dom change listener, call it
			if t.domChangeHandler != nil {
				t.domChangeHandler(t, nodeChangeEvent)
			}
			t.lastNodeChangeTime = time.Now()
		}
	}
}

// handle node change events, updating, inserting invalidating and removing
func (t *Tab) handleNodeChange(change *NodeChangeEvent) {
	switch change.EventType {
	case DocumentUpdatedEvent:
		t.handleDocumentUpdated()
	case SetChildNodesEvent:
		t.setNodesWg.Add(1)
		go t.handleSetChildNodes(change.ParentNodeId, change.Nodes)
	case AttributeModifiedEvent:
		if ele, ok := t.getElement(change.NodeId); ok {
			ele.updateAttribute(change.Name, change.Value)
		}
	case AttributeRemovedEvent:
		if ele, ok := t.getElement(change.NodeId); ok {
			ele.removeAttribute(change.Name)
		}
	case CharacterDataModifiedEvent:
		if ele, ok := t.getElement(change.NodeId); ok {
			ele.updateCharacterData(change.CharacterData)
		}
	case ChildNodeCountUpdatedEvent:
		if ele, ok := t.getElement(change.NodeId); ok {
			ele.updateChildNodeCount(change.ChildNodeCount)
			// request the child nodes
			t.requestChildNodes(change.NodeId, -1)
		}
	case ChildNodeInsertedEvent:
		t.handleChildNodeInserted(change.ParentNodeId, change.Node)
	case ChildNodeRemovedEvent:
		t.handleChildNodeRemoved(change.ParentNodeId, change.NodeId)
	}

}

// Handles the document updated event. This occurs after a navigation or redirect.
// This is a destructive action which invalidates all document nodeids and their children.
// We loop through our current list of elements and invalidate them so any references
// can check if they are valid or not. We then recreate the elements map. Finally, if we
// are navigating, we want to block Navigate from returning until we have a valid document,
// so we use the docUpdateCh to signal when complete.
func (t *Tab) handleDocumentUpdated() {
	// set all elements as invalid and destroy the Elements map.
	t.eleMutex.Lock()
	for _, ele := range t.elements {
		ele.setInvalidated(true)
	}
	t.elements = make(map[int]*Element)
	t.eleMutex.Unlock()
	t.documentUpdated()
	// notify if navigating that we received the document update event.
	if t.isNavigating == true {
		t.docUpdateCh <- struct{}{} // notify listeners document was updated
	}
}

// setChildNode event handling will add nodes to our elements map and update
// the parent reference Children
func (t *Tab) handleSetChildNodes(parentNodeId int, nodes []*gcdapi.DOMNode) {
	for _, node := range nodes {

		t.addNodes(node)
	}
	parent, ready := t.getElement(parentNodeId)
	if ready {
		parent.addChildren(nodes)
	}
	t.lastNodeChangeTime = time.Now()
	t.setNodesWg.Done()
}

// update parent with new child node and add nodes.
func (t *Tab) handleChildNodeInserted(parentNodeId int, node *gcdapi.DOMNode) {
	t.debugf("child node inserted: id: %d\n", node.NodeId)
	t.addNodes(node)

	parent, ready := t.getElement(parentNodeId)
	if ready {
		parent.addChild(node)
	}
	t.lastNodeChangeTime = time.Now()
}

// update ParentNodeId to remove child and iterate over Children recursively and invalidate them.
func (t *Tab) handleChildNodeRemoved(parentNodeId, nodeId int) {
	t.debugf("child node REMOVED: %d\n", nodeId)
	ele, ok := t.getElement(nodeId)
	if !ok {
		return
	}
	ele.setInvalidated(true)
	parent, ready := t.getElement(parentNodeId)
	if ready {
		parent.removeChild(ele.node)
	}
	t.invalidateChildren(ele.node)

	t.eleMutex.Lock()
	delete(t.elements, nodeId)
	t.eleMutex.Unlock()
}

// when a childNodeRemoved event occurs, we need to set each child
// to invalidated and remove it from our elements map.
func (t *Tab) invalidateChildren(node *gcdapi.DOMNode) {
	// invalidate & remove ContentDocument node and children
	if node.ContentDocument != nil {
		ele, ok := t.getElement(node.ContentDocument.NodeId)
		if ok {
			t.invalidateRemove(ele)
			t.invalidateChildren(node.ContentDocument)
		}
	}

	if node.Children == nil {
		return
	}

	// invalidate node.Children
	for _, child := range node.Children {
		ele, ok := t.getElement(child.NodeId)
		if !ok {
			continue
		}
		t.invalidateRemove(ele)
		// recurse and remove children of this node
		t.invalidateChildren(ele.node)
	}
}

// Sets the element as invalid and removes it from our elements map
func (t *Tab) invalidateRemove(ele *Element) {
	t.debugf("invalidating nodeId: %d\n", ele.id)
	ele.setInvalidated(true)
	t.eleMutex.Lock()
	delete(t.elements, ele.id)
	t.eleMutex.Unlock()
}

// the entire document has been invalidated, request all nodes again
func (t *Tab) documentUpdated() {
	t.debugf("document updated, refreshed")
	t.getDocument()
}

// Ask the debugger service for child nodes
func (t *Tab) requestChildNodes(nodeId, depth int) {
	_, err := t.DOM.RequestChildNodes(nodeId, depth)
	if err != nil {
		t.debugf("error requesting child nodes: %s\n", err)
	}
}

// Called if the element is known about but not yet populated. If it is not
// known, we create a new element. If it is known we populate it and return it.
func (t *Tab) nodeToElement(node *gcdapi.DOMNode) *Element {
	if ele, ok := t.getElement(node.NodeId); ok {
		ele.populateElement(node)
		return ele
	}
	newEle := newReadyElement(t, node)
	return newEle
}

// safely returns the element by looking it up by nodeId
func (t *Tab) getElement(nodeId int) (*Element, bool) {
	t.eleMutex.RLock()
	defer t.eleMutex.RUnlock()
	ele, ok := t.elements[nodeId]
	return ele, ok
}

// Safely adds the nodes in the document to our list of elements
// iterates over children and contentdocuments (if they exist)
// Calls requestchild nodes for each node so we can receive setChildNode
// events for even more nodes
func (t *Tab) addNodes(node *gcdapi.DOMNode) {
	newEle := t.nodeToElement(node)
	t.eleMutex.Lock()
	t.elements[newEle.id] = newEle
	t.eleMutex.Unlock()
	//log.Printf("Added new element: %s\n", newEle)
	t.requestChildNodes(newEle.id, -1)
	if node.Children != nil {
		// add child nodes
		for _, v := range node.Children {
			t.addNodes(v)
		}
	}
	if node.ContentDocument != nil {
		t.addNodes(node.ContentDocument)
	}
	t.lastNodeChangeTime = time.Now()
}

func (t *Tab) debugf(format string, args ...interface{}) {
	if t.debug {
		log.Printf(format, args...)
	}
}
