// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;
import {Test} from "forge-std/Test.sol";
import {XmrSwapV3} from "../src/XmrSwapV3.sol";
import {SeshRegistry} from "../src/registry/SeshRegistry.sol";
import {ExitLease} from "../src/exits/ExitLease.sol";
import {ExitPay, ExitPayForwarder} from "../src/exits/ExitPay.sol";
import {MockUSDC} from "../src/MockUSDC.sol";
import {MockAggregatorV3} from "../src/MockAggregatorV3.sol";

/// XMR -> ETH swap whose claim buys exit time: the claimer (XMR seller) claims
/// to an ExitPay address that fixes listing, hours, buyer .sesh, max cost and
/// where change goes.
contract ExitPayTest is Test {
    XmrSwapV3 c;
    SeshRegistry reg;
    ExitLease el;
    ExitPay ep;
    MockUSDC usdc;

    address maker = address(0xA11CE); // locks ETH (swap owner)
    uint256 bobPk = 0xB0B5EC; // XMR seller, claimer
    address bob;
    address relayer = address(0x7E1A7);
    address seller = address(0x5E11);
    address payout = address(0x9A70);
    address change = address(0xC4A6);
    uint256 listing;

    uint256 constant BOB_S = 0x088fba2ab5511efb59a49f0f8e1ceceeb302ad1f60a4007702607c789719eaf5;
    uint256 constant BOB_K = 0xd51fb7a1b6932d11ee69a0e977c3c3bc0f3be706735daf55f6c11050b5c0c96b;
    uint256 constant ALICE_S = 0x0a4c454746ce6afc23d46643a0f26aba063b6af57206da911d09552be6b47a25;
    uint256 constant ALICE_K = 0x01c2a86a450bfac25beed06f52bc6b0ce204bcff0671285c93269f341c8e0254;
    uint64 constant D1 = 1 hours;
    uint64 constant D2 = 2 hours;
    uint64 constant G = 6 hours;
    uint256 constant WEI_H = 0.00002 ether;
    uint256 constant VALUE = 0.0005 ether;
    string constant EXIT_SESH = "wo7xwuzostufug8ptrth5natahx53nug9jsazm783nere8rj68ky.sesh";
    string constant BSESH = "rgwybw99m4d464rbi9z95a7a364n1hauo74ip486ssckf1agw7zo.sesh";

    function setUp() public {
        vm.warp(1_000_000);
        bob = vm.addr(bobPk);
        usdc = new MockUSDC();
        c = new XmrSwapV3();
        reg = new SeshRegistry(
            SeshRegistry.Params({
                swap: address(c),
                siteMinBond: 0.0002 ether,
                makerMinBond: 0.002 ether,
                capFactor: 5,
                penaltyBps: 500,
                minPenalty: 0.00005 ether,
                challengeBond: 0.0001 ether,
                exitDelay: 3 days,
                challengeWindow: 2 days,
                minAttest: 10 minutes,
                maxAttest: 30 minutes,
                usdc: address(usdc),
                usdcDecimals: 6,
                ethUsdFeed: address(new MockAggregatorV3("ETH / USD", 3000e8)),
                maxFeedAge: 2 days
            })
        );
        el = new ExitLease(reg, address(usdc));
        ep = new ExitPay(el, address(c));
        vm.deal(seller, 1 ether);
        vm.deal(maker, 10 ether);
        vm.prank(seller);
        listing = reg.register{value: 0.0002 ether}(EXIT_SESH, "exit", "exit; price=0.00002 ETH/h");
        vm.prank(seller);
        el.setOffer(listing, payout, uint128(WEI_H), 0.1e6, 24);
    }

    function target(uint32 h, uint256 maxCost) internal view returns (ExitPay.Target memory) {
        return ExitPay.Target(address(0), change, listing, h, maxCost, bytes32(uint256(77)), BSESH);
    }

    function lockSwap(bytes32 salt) internal returns (XmrSwapV3.Swap memory s, bytes32 id) {
        s = XmrSwapV3.Swap(maker, bob, BOB_K, ALICE_K, uint64(block.timestamp) + D1,
            uint64(block.timestamp) + D1 + D2, G, address(0), VALUE, 0, salt);
        vm.prank(maker);
        id = c.lock{value: VALUE}(bob, BOB_K, ALICE_K, D1, D2, G, address(0), VALUE, 0, salt);
        vm.prank(maker);
        c.setReady(s);
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.intentDigest(id));
        c.claimIntent(s, v, r, ss);
    }

    function relayedClaim(XmrSwapV3.Swap memory s, bytes32 id, address to, uint256 fee) internal {
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.claimDigest(id, to, address(0), fee));
        vm.prank(relayer);
        c.claimRelayed(s, BOB_S, to, address(0), fee, v, r, ss);
    }

    function test_claimBuysHoursInTheSameTx() public {
        ExitPay.Target memory t = target(10, 0.0003 ether);
        address f = ep.deploy(t);
        assertEq(f, ep.payAddress(t));
        (XmrSwapV3.Swap memory s, bytes32 id) = lockSwap(bytes32(uint256(1)));
        uint256 fee = 0.00001 ether;
        vm.expectEmit(true, true, true, true, address(el));
        emit ExitLease.Leased(0, listing, f, payout, address(0), 10 * WEI_H, 10, 1_000_000, 1_000_000 + 10 hours, BSESH);
        relayedClaim(s, id, f, fee);
        assertEq(payout.balance, 10 * WEI_H, "seller paid");
        assertEq(change.balance, VALUE - fee - 10 * WEI_H, "change to the XMR seller");
        assertEq(relayer.balance, fee);
        assertEq(f.balance, 0);
        assertEq(el.activeUntil(listing, BSESH), 1_000_000 + 10 hours);
        assertTrue(ExitPayForwarder(payable(f)).done());
        assertEq(bob.balance, 0, "nothing to the claimer's own address");
    }

    function test_claimBeforeDeployThenSettle() public {
        ExitPay.Target memory t = target(5, 0.0001 ether);
        address f = ep.payAddress(t);
        assertEq(f.code.length, 0);
        (XmrSwapV3.Swap memory s, bytes32 id) = lockSwap(bytes32(uint256(2)));
        relayedClaim(s, id, f, 0);
        assertEq(f.balance, VALUE, "money waits at the address");
        vm.prank(address(0xDEAD)); // anyone may settle
        ep.settle(t);
        assertEq(payout.balance, 5 * WEI_H);
        assertEq(change.balance, VALUE - 5 * WEI_H);
        assertEq(el.activeUntil(listing, BSESH), 1_000_000 + 5 hours);
        ep.settle(t); // idempotent
        assertEq(payout.balance, 5 * WEI_H);
    }

    function test_relayerCannotRedirect() public {
        ExitPay.Target memory t = target(1, 0.0001 ether);
        address f = ep.deploy(t);
        (XmrSwapV3.Swap memory s, bytes32 id) = lockSwap(bytes32(uint256(3)));
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.claimDigest(id, f, address(0), 0));
        address other = ep.payAddress(ExitPay.Target(address(0), relayer, listing, 1, 0.0001 ether, bytes32(uint256(77)), BSESH));
        vm.startPrank(relayer);
        vm.expectRevert(XmrSwapV3.BadSignature.selector);
        c.claimRelayed(s, BOB_S, other, address(0), 0, v, r, ss);
        vm.expectRevert(XmrSwapV3.BadSignature.selector);
        c.claimRelayed(s, BOB_S, relayer, address(0), 0, v, r, ss);
        c.claimRelayed(s, BOB_S, f, address(0), 0, v, r, ss);
        vm.stopPrank();
        assertEq(el.activeUntil(listing, BSESH), 1_000_000 + 1 hours);
    }

    function test_everyFieldChangesTheAddress() public view {
        ExitPay.Target memory t = target(3, 0.0001 ether);
        address a = ep.payAddress(t);
        ExitPay.Target memory u = target(3, 0.0001 ether);
        u.buyerSesh = EXIT_SESH;
        assertTrue(ep.payAddress(u) != a);
        u = target(4, 0.0001 ether);
        assertTrue(ep.payAddress(u) != a);
        u = target(3, 0.0002 ether);
        assertTrue(ep.payAddress(u) != a);
        u = target(3, 0.0001 ether);
        u.refundTo = relayer;
        assertTrue(ep.payAddress(u) != a);
        u = target(3, 0.0001 ether);
        u.listingId = listing + 1;
        assertTrue(ep.payAddress(u) != a);
        u = target(3, 0.0001 ether);
        u.ref = bytes32(uint256(78));
        assertTrue(ep.payAddress(u) != a);
    }

    function test_priceAboveMaxRefundsEverything() public {
        ExitPay.Target memory t = target(10, 10 * WEI_H);
        address f = ep.deploy(t);
        vm.prank(seller);
        el.setOffer(listing, payout, uint128(WEI_H * 2), 0, 24); // seller raises the price
        (XmrSwapV3.Swap memory s, bytes32 id) = lockSwap(bytes32(uint256(4)));
        relayedClaim(s, id, f, 0);
        assertEq(payout.balance, 0);
        assertEq(change.balance, VALUE);
        assertEq(el.activeUntil(listing, BSESH), 0);
        assertFalse(ExitPayForwarder(payable(f)).done());
    }

    function test_offerGoneRefundsEverything() public {
        ExitPay.Target memory t = target(2, 0.0001 ether);
        address f = ep.deploy(t);
        vm.prank(seller);
        el.clearOffer(listing);
        (XmrSwapV3.Swap memory s, bytes32 id) = lockSwap(bytes32(uint256(5)));
        relayedClaim(s, id, f, 0);
        assertEq(change.balance, VALUE);
        assertEq(payout.balance, 0);
    }

    function test_tooFewProceedsRefunds() public {
        ExitPay.Target memory t = target(24, 1 ether); // 24h costs 0.00048, proceeds 0.0005 - fee
        address f = ep.deploy(t);
        (XmrSwapV3.Swap memory s, bytes32 id) = lockSwap(bytes32(uint256(6)));
        relayedClaim(s, id, f, 0.0001 ether);
        assertEq(payout.balance, 0);
        assertEq(change.balance, VALUE - 0.0001 ether);
    }

    function test_makerRefundStillWorks() public {
        ExitPay.Target memory t = target(1, 0.0001 ether);
        ep.deploy(t);
        XmrSwapV3.Swap memory s = XmrSwapV3.Swap(maker, bob, BOB_K, ALICE_K, uint64(block.timestamp) + D1,
            uint64(block.timestamp) + D1 + D2, G, address(0), VALUE, 0, bytes32(uint256(7)));
        vm.prank(maker);
        c.lock{value: VALUE}(bob, BOB_K, ALICE_K, D1, D2, G, address(0), VALUE, 0, bytes32(uint256(7)));
        uint256 before = maker.balance;
        vm.warp(s.t2);
        vm.prank(maker);
        c.refund(s, ALICE_S);
        assertEq(maker.balance, before + VALUE);
        assertEq(el.activeUntil(listing, BSESH), 0);
    }

    function test_stacksOnExistingTime() public {
        ExitPay.Target memory t = target(2, 0.0001 ether);
        address f = ep.deploy(t);
        (XmrSwapV3.Swap memory s, bytes32 id) = lockSwap(bytes32(uint256(8)));
        relayedClaim(s, id, f, 0);
        // A second swap with its own ref gets its own address; its hours start
        // when the first ones end.
        ExitPay.Target memory t2 = target(2, 0.0001 ether);
        t2.ref = bytes32(uint256(99));
        address f2 = ep.deploy(t2);
        (s, id) = lockSwap(bytes32(uint256(9)));
        relayedClaim(s, id, f2, 0);
        assertEq(el.activeUntil(listing, BSESH), 1_000_000 + 4 hours);
        // Paying the same address twice never buys twice: the extra goes back.
        (s, id) = lockSwap(bytes32(uint256(12)));
        relayedClaim(s, id, f, 0);
        assertEq(el.activeUntil(listing, BSESH), 1_000_000 + 4 hours);
        assertEq(change.balance, 3 * VALUE - 4 * WEI_H);
    }

    function test_usdcProceedsNeedSettle() public {
        usdc.mint(maker, 10e6);
        ExitPay.Target memory t = ExitPay.Target(address(usdc), change, listing, 3, 0.3e6, bytes32(uint256(78)), BSESH);
        address f = ep.deploy(t);
        bytes32 salt = bytes32(uint256(10));
        XmrSwapV3.Swap memory s = XmrSwapV3.Swap(maker, bob, BOB_K, ALICE_K, uint64(block.timestamp) + D1,
            uint64(block.timestamp) + D1 + D2, G, address(usdc), 1e6, 0, salt);
        vm.startPrank(maker);
        usdc.approve(address(c), 1e6);
        bytes32 id = c.lock(bob, BOB_K, ALICE_K, D1, D2, G, address(usdc), 1e6, 0, salt);
        c.setReady(s);
        vm.stopPrank();
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.intentDigest(id));
        c.claimIntent(s, v, r, ss);
        relayedClaim(s, id, f, 0);
        assertEq(usdc.balanceOf(f), 1e6);
        ExitPayForwarder(payable(f)).settle();
        assertEq(usdc.balanceOf(payout), 0.3e6);
        assertEq(usdc.balanceOf(change), 0.7e6);
        assertEq(el.activeUntil(listing, BSESH), 1_000_000 + 3 hours);
    }

    function test_deployRejectsBadTargets() public {
        vm.expectRevert(ExitPay.BadParams.selector);
        ep.deploy(ExitPay.Target(address(0), address(0), listing, 1, 1, bytes32(0), BSESH));
        vm.expectRevert(ExitPay.BadParams.selector);
        ep.deploy(ExitPay.Target(address(0), change, listing, 0, 1, bytes32(0), BSESH));
        vm.expectRevert(ExitPay.BadParams.selector);
        ep.deploy(ExitPay.Target(address(0), change, listing, 1, 0, bytes32(0), BSESH));
    }

    function testFuzz_neverLosesMoney(uint32 h, uint96 maxCost, uint64 fee) public {
        h = uint32(bound(h, 1, 30));
        uint256 mc = bound(maxCost, 1, 0.001 ether);
        uint256 f_ = bound(fee, 0, VALUE);
        ExitPay.Target memory t = target(h, mc);
        address f = ep.deploy(t);
        (XmrSwapV3.Swap memory s, bytes32 id) = lockSwap(bytes32(uint256(11)));
        relayedClaim(s, id, f, f_);
        assertEq(f.balance, 0);
        assertEq(payout.balance + change.balance + relayer.balance, VALUE);
    }
}
